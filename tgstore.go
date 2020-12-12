/*
Package tgstore implements an encrypted object storage system with unlimited
space backed by Telegram.
*/
package tgstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"gopkg.in/tucnak/telebot.v2"
)

// TGStore is the top-level struct of this project.
//
// It is highly recommended not to modify the value of any field of the
// `TGStore` after calling any methods of it, which will cause unpredictable
// problems.
//
// The new instances of the `TGStore` should only be created by calling the
// `New`.
type TGStore struct {
	// BotAPIEndpoint is the endpoint of the Telegram Bot API.
	//
	// You might prefer to use your local Telegram Bot API server. See
	// https://core.telegram.org/bots/api#using-a-local-bot-api-server for
	// benefits.
	//
	// Default value: "https://api.telegram.org"
	BotAPIEndpoint string `mapstructure:"bot_api_endpoint"`

	// BotToken is the Telegram bot token.
	//
	// Default value: ""
	BotToken string `mapstructure:"bot_token"`

	// ChatID is the ID of the Telegram chat used to store the objects to be
	// uploaded.
	//
	// It is ok to change the `ChatID` if you want. The objects that have
	// already been uploaded are not affected.
	//
	// Default value: 0
	ChatID int64 `mapstructure:"chat_id"`

	// MaxFileBytes is the maximum number of bytes allowed for a Telegram
	// file to have.
	//
	// The `MaxFileBytes` must be at least 20971520 (20 MiB).
	//
	// It is ok to change the `MaxFileBytes` if you want. The objects that
	// have already been uploaded are not affected.
	//
	// Default value: 20971492
	MaxFileBytes int `mapstructure:"max_file_bytes"`

	// HTTPClient is the `http.Client` used to communicate with the Telegram
	// Bot API.
	//
	// Default value: `http.DefaultClient`
	HTTPClient *http.Client `mapstructure:"-"`

	loadOnce              sync.Once
	loadError             error
	bot                   *telebot.Bot
	chat                  *telebot.Chat
	maxObjectContentBytes int64
}

// New returns a new instance of the `TGStore` with default field values.
//
// The `New` is the only function that creates new instances of the `TGStore`
// and keeps everything working.
func New() *TGStore {
	return &TGStore{
		BotAPIEndpoint: "https://api.telegram.org",
		MaxFileBytes:   20 << 20,
		HTTPClient:     http.DefaultClient,
	}
}

// load loads the stuff of the tgs up.
func (tgs *TGStore) load() {
	if tgs.bot, tgs.loadError = telebot.NewBot(telebot.Settings{
		URL:      tgs.BotAPIEndpoint,
		Token:    tgs.BotToken,
		Reporter: func(error) {},
		Client:   tgs.HTTPClient,
	}); tgs.loadError != nil {
		tgs.loadError = fmt.Errorf(
			"failed to create telegram bot: %v",
			tgs.loadError,
		)
		return
	}

	if tgs.chat, tgs.loadError = tgs.bot.ChatByID(
		strconv.FormatInt(tgs.ChatID, 10),
	); tgs.loadError != nil {
		tgs.loadError = fmt.Errorf(
			"failed to get telegram chat: %v",
			tgs.loadError,
		)
		return
	}

	if tgs.MaxFileBytes < 20<<20 {
		tgs.loadError = errors.New("invalid max file bytes")
		return
	}

	tgs.maxObjectContentBytes = int64(tgs.MaxFileBytes)
	tgs.maxObjectContentBytes /= objectEncryptedChunkSize * objectChunkSize
}

// UploadObject uploads the content to the cloud.
//
// The lenth of the key must be 16.
func (tgs *TGStore) UploadObject(
	ctx context.Context,
	key []byte,
	content io.Reader,
) (*Object, error) {
	return tgs.AppendObject(ctx, "", key, content)
}

// AppendObject appends the content to the object targeted by the id.
//
// The lenth of the key must be 16.
func (tgs *TGStore) AppendObject(
	ctx context.Context,
	id string,
	key []byte,
	content io.Reader,
) (*Object, error) {
	tgs.loadOnce.Do(tgs.load)
	if tgs.loadError != nil {
		return nil, tgs.loadError
	}

	if content == nil {
		content = bytes.NewReader(nil)
	}

	buf := bytes.Buffer{}
	for {
		if _, err := io.CopyN(&buf, content, 1); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, err
		}

		object, err := tgs.appendObject(
			ctx,
			id,
			key,
			io.LimitReader(
				io.MultiReader(&buf, content),
				tgs.maxObjectContentBytes,
			),
		)
		if err != nil {
			return nil, err
		}

		id = object.ID
	}

	if id != "" {
		return tgs.DownloadObject(ctx, id, key)
	}

	return tgs.appendObject(ctx, id, key, content)
}

// appendObject appends the content to the object targeted by the id.
//
// The lenth of the key must be 16.
func (tgs *TGStore) appendObject(
	ctx context.Context,
	id string,
	key []byte,
	content io.Reader,
) (*Object, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}

	var (
		contents []*objectContent
		size     int64
		hashFunc = sha256.New()
	)

	content = io.TeeReader(content, hashFunc)

	if id != "" {
		object, err := tgs.DownloadObject(ctx, id, key)
		if err != nil {
			return nil, err
		}

		contents = object.contents
		size = object.Size
		if len(contents) > 0 {
			lc := contents[len(contents)-1]
			if lc.Size < 8<<20 {
				lcr, err := lc.newReader(ctx, aead, 0)
				if err != nil {
					return nil, err
				}
				defer lcr.Close()

				content = io.MultiReader(lcr, content)

				contents = contents[:len(contents)-1]
				size -= lc.Size
			}
		}

		if err := hashFunc.(encoding.BinaryUnmarshaler).
			UnmarshalBinary(object.hashMidstate); err != nil {
			return nil, err
		}
	}

	var contentSize int64

	pipeReader, pipeWriter := io.Pipe()
	go func() (err error) {
		defer func() {
			pipeWriter.CloseWithError(err)
		}()

		var (
			buf   = make([]byte, objectEncryptedChunkSize)
			nonce = make([]byte, chacha20poly1305.NonceSize)
		)

		for {
			n, err := io.ReadFull(content, buf[:objectChunkSize])
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				} else if !errors.Is(err, io.ErrUnexpectedEOF) {
					return err
				}
			}

			if err := readNonce(nonce); err != nil {
				return err
			}

			if _, err := pipeWriter.Write(nonce); err != nil {
				return err
			}

			if _, err := pipeWriter.Write(aead.Seal(
				buf[:0],
				nonce,
				buf[:n],
				nil,
			)); err != nil {
				return err
			}

			size += int64(n)
			contentSize += int64(n)
		}

		return nil
	}()

	buf := bytes.Buffer{}
	if _, err := io.CopyN(&buf, pipeReader, 1); err == nil {
		contentID, err := tgs.uploadTelegramFile(
			ctx,
			io.MultiReader(&buf, pipeReader),
		)
		if err != nil {
			pipeReader.CloseWithError(err)
			return nil, err
		}

		contents = append(contents, &objectContent{
			ID:   contentID,
			Size: contentSize,
		})
	} else if !errors.Is(err, io.EOF) {
		pipeReader.CloseWithError(err)
		return nil, err
	}

	hashMidstate, err := hashFunc.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, err
	}

	metadataJSON, err := json.Marshal(objectMetadata{
		Contents:     contents,
		Size:         size,
		HashMidstate: base64.StdEncoding.EncodeToString(hashMidstate),
	})
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, chacha20poly1305.NonceSize)
	if err := readNonce(nonce); err != nil {
		return nil, err
	}

	if id, err = tgs.uploadTelegramFile(
		ctx,
		io.MultiReader(
			bytes.NewReader(nonce),
			bytes.NewReader(aead.Seal(
				nil,
				nonce,
				metadataJSON,
				nil,
			)),
		),
	); err != nil {
		return nil, err
	}

	return &Object{
		ID:           id,
		Size:         size,
		Checksum:     hashFunc.Sum(nil),
		aead:         aead,
		contents:     contents,
		hashMidstate: hashMidstate,
	}, nil
}

// DownloadObject downloads the object targeted by the id from the cloud. It
// returns `os.ErrNotExist` if not found.
//
// The lenth of the key must be 16.
func (tgs *TGStore) DownloadObject(
	ctx context.Context,
	id string,
	key []byte,
) (*Object, error) {
	tgs.loadOnce.Do(tgs.load)
	if tgs.loadError != nil {
		return nil, tgs.loadError
	}

	if id == "" {
		return nil, os.ErrNotExist
	}

	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}

	rc, err := tgs.downloadTelegramFile(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	metadataJSON, err := ioutil.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	nonce := metadataJSON[:chacha20poly1305.NonceSize]
	metadataJSON = metadataJSON[chacha20poly1305.NonceSize:]
	if metadataJSON, err = aead.Open(
		metadataJSON[:0],
		nonce,
		metadataJSON,
		nil,
	); err != nil {
		return nil, err
	}

	metadata := &objectMetadata{}
	if err := json.Unmarshal(metadataJSON, metadata); err != nil {
		return nil, err
	}

	hashMidstate, err := base64.StdEncoding.
		DecodeString(metadata.HashMidstate)
	if err != nil {
		return nil, err
	}

	hashFunc := sha256.New()
	if err := hashFunc.(encoding.BinaryUnmarshaler).
		UnmarshalBinary(hashMidstate); err != nil {
		return nil, err
	}

	return &Object{
		ID:           id,
		Size:         metadata.Size,
		Checksum:     hashFunc.Sum(nil),
		aead:         aead,
		contents:     metadata.Contents,
		hashMidstate: hashMidstate,
	}, nil
}

// uploadTelegramFile uploads the content to the Telegram.
func (tgs *TGStore) uploadTelegramFile(
	ctx context.Context,
	content io.Reader,
) (string, error) {
	startTime := time.Now()

	cr := &countReader{
		r: content,
	}

Upload:
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	m, err := tgs.bot.Send(tgs.chat, &telebot.Document{
		File: telebot.FromReader(cr),
	})
	if err != nil {
		if cr.c == 0 &&
			isRetryableTelegramBotAPIError(err) &&
			time.Now().Sub(startTime) < time.Minute {
			time.Sleep(time.Second)
			goto Upload
		}

		return "", err
	}

	return m.Document.FileID, nil
}

// downloadTelegramFile downloads the file targeted by the id from the Telegram.
// It returns `os.ErrNotExist` if not found.
func (tgs *TGStore) downloadTelegramFile(
	ctx context.Context,
	id string,
	offset int64,
) (io.ReadCloser, error) {
	startTime := time.Now()

Ready:
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	fileURL, err := tgs.bot.FileURLByID(id)
	if err != nil {
		if strings.Contains(err.Error(), "Not Found") {
			return nil, os.ErrNotExist
		}

		if isRetryableTelegramBotAPIError(err) &&
			time.Now().Sub(startTime) < time.Minute {
			time.Sleep(time.Second)
			goto Ready
		}

		return nil, err
	}

Download:
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fileURL,
		nil,
	)
	if err != nil {
		return nil, err
	}

	res, err := tgs.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		if time.Now().Sub(startTime) < time.Minute {
			time.Sleep(time.Second)
			goto Download
		}

		b, err := ioutil.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("GET %s: %s: %s", req.URL, res.Status, b)
	}

	return res.Body, nil
}

// isRetryableTelegramBotAPIError reports whether the err is a retryable
// Telegram Bot API error.
func isRetryableTelegramBotAPIError(err error) bool {
	em := err.Error()
	return strings.Contains(em, "Bad Request") ||
		strings.Contains(em, "Too Many Requests") ||
		strings.Contains(em, "Bad Gateway") ||
		strings.Contains(em, "Service Unavailable") ||
		strings.Contains(em, "Gateway Timeout")
}

// readNonceMutex is the `sync.Mutex` for the `readNonce`.
var readNonceMutex sync.Mutex

// readNonce reads nonce to the b.
func readNonce(b []byte) error {
	readNonceMutex.Lock()
	defer readNonceMutex.Unlock()
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
	_, err := rand.Read(b[8:])
	return err
}

// countReader is used to count the number of bytes read from the underlying
// `io.Reader`.
type countReader struct {
	r io.Reader
	c int64
}

// Read implements the `io.Reader`.
func (cr *countReader) Read(b []byte) (int, error) {
	n, err := cr.r.Read(b)
	cr.c += int64(n)
	return n, err
}
