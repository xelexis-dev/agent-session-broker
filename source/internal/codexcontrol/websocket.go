//go:build darwin || linux

package codexcontrol

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxMessageBytes = 8 << 20
const maxFrameBytes = 8 << 20
const maxHeaderBytes = 8 << 10

var (
	ErrProtocol = errors.New("codex control protocol failure")
	ErrClosed   = errors.New("codex control connection closed")
)

type websocket struct {
	r *bufio.Reader
	w io.Writer
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func (w *websocket) upgrade() error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ErrProtocol
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	request := "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n\r\n"
	if err := writeAll(w.w, []byte(request)); err != nil {
		return err
	}
	return readUpgrade(w.r, key)
}
func readUpgrade(r *bufio.Reader, key string) error {
	var header []byte
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return err
		}
		if len(header)+len(line) > maxHeaderBytes || !bytes.HasSuffix(line, []byte("\r\n")) {
			return ErrProtocol
		}
		header = append(header, line...)
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), &http.Request{Method: http.MethodGet})
	if err != nil {
		return ErrProtocol
	}
	upgrade := false
	for _, token := range strings.Split(response.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			upgrade = true
		}
	}
	if response.StatusCode != 101 || !response.ProtoAtLeast(1, 1) || !upgrade || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") || len(response.Header.Values("Sec-WebSocket-Accept")) != 1 || response.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) || response.Header.Get("Sec-WebSocket-Extensions") != "" || response.Header.Get("Sec-WebSocket-Protocol") != "" {
		return ErrProtocol
	}
	return nil
}
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func (w *websocket) writeFrame(op byte, data []byte) error {
	if len(data) > maxFrameBytes || (op >= 8 && len(data) > 125) {
		return ErrTooLarge
	}
	header := []byte{0x80 | op}
	n := len(data)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 65535:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return ErrProtocol
	}
	header = append(header, mask[:]...)
	if err := writeAll(w.w, header); err != nil {
		return err
	}
	var chunk [4096]byte
	for offset := 0; offset < n; {
		count := min(n-offset, len(chunk))
		for i := 0; i < count; i++ {
			chunk[i] = data[offset+i] ^ mask[(offset+i)%4]
		}
		if err := writeAll(w.w, chunk[:count]); err != nil {
			return err
		}
		offset += count
	}
	return nil
}
func (w *websocket) readFrame() (byte, bool, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(w.r, head[:]); err != nil {
		return 0, false, nil, err
	}
	op, fin := head[0]&15, head[0]&0x80 != 0
	if head[0]&0x70 != 0 || head[1]&0x80 != 0 || (op != 0 && op != 1 && op != 8 && op != 9 && op != 10) {
		return 0, false, nil, ErrProtocol
	}
	n := uint64(head[1] & 127)
	switch n {
	case 126:
		var size [2]byte
		if _, err := io.ReadFull(w.r, size[:]); err != nil {
			return 0, false, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(size[:]))
		if n < 126 {
			return 0, false, nil, ErrProtocol
		}
	case 127:
		var size [8]byte
		if _, err := io.ReadFull(w.r, size[:]); err != nil {
			return 0, false, nil, err
		}
		n = binary.BigEndian.Uint64(size[:])
		if n < 65536 || n>>63 != 0 {
			return 0, false, nil, ErrProtocol
		}
	}
	if op >= 8 && (!fin || n > 125) {
		return 0, false, nil, ErrProtocol
	}
	if n > maxFrameBytes {
		return 0, false, nil, ErrTooLarge
	}
	// Allocate for bytes actually received, not an untrusted declared length.
	data, err := io.ReadAll(io.LimitReader(w.r, int64(n)))
	if err != nil {
		return 0, false, nil, err
	}
	if uint64(len(data)) != n {
		return 0, false, nil, io.ErrUnexpectedEOF
	}
	return op, fin, data, nil
}
func (w *websocket) readMessage() ([]byte, error) {
	var message []byte
	started := false
	for {
		op, fin, data, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 8:
			if len(data) == 1 {
				return nil, ErrProtocol
			}
			if len(data) >= 2 {
				code := binary.BigEndian.Uint16(data)
				if !((code >= 1000 && code <= 1014 && code != 1004 && code != 1005 && code != 1006) || (code >= 3000 && code <= 4999)) || !utf8.Valid(data[2:]) {
					return nil, ErrProtocol
				}
			}
			if err := w.writeFrame(8, data); err != nil {
				return nil, err
			}
			return nil, ErrClosed
		case 9:
			if err := w.writeFrame(10, data); err != nil {
				return nil, err
			}
			continue
		case 10:
			continue
		case 1:
			if started {
				return nil, ErrProtocol
			}
			started = true
		case 0:
			if !started {
				return nil, ErrProtocol
			}
		}
		if len(data) > maxMessageBytes-len(message) {
			return nil, ErrTooLarge
		}
		message = append(message, data...)
		if fin {
			if !utf8.Valid(message) {
				return nil, ErrProtocol
			}
			return message, nil
		}
	}
}
