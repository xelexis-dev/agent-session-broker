//go:build darwin || linux

package codexcontrol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFragmentedTextWithInterleavedPing(t *testing.T) {
	// A Unicode character may be split across fragments; ping must not end the text.
	input := []byte{0x01, 3, 'o', 'k', ' ', 0x89, 1, 'p', 0x00, 2, 0xf0, 0x9f, 0x8a, 0, 0x80, 2, 0x8c, 0x8d}
	var output bytes.Buffer
	w := websocket{r: bufio.NewReader(bytes.NewReader(input)), w: &output}
	got, err := w.readMessage()
	if err != nil || string(got) != "ok 🌍" {
		t.Fatalf("valid fragmented text was not assembled: %v", err)
	}
	pong := output.Bytes()
	if len(pong) != 7 || pong[0] != 0x8a || pong[1] != 0x81 || pong[6]^pong[2] != 'p' {
		t.Fatal("ping did not produce a masked pong")
	}
}

func serverFrame(op byte, fin bool, body []byte) []byte {
	h := []byte{op}
	if fin {
		h[0] |= 0x80
	}
	switch n := len(body); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 65535:
		h = append(h, 126, byte(n>>8), byte(n))
	default:
		h = append(h, 127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	return append(h, body...)
}

type tinyReader struct{ io.Reader }

func (r tinyReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), 1)]) }

type tinyWriter struct{ bytes.Buffer }

func (w *tinyWriter) Write(p []byte) (int, error) { return w.Buffer.Write(p[:min(len(p), 1)]) }

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
func readClientFrame(r io.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[0]&0x80 == 0 || h[1]&0x80 == 0 {
		return 0, nil, ErrProtocol
	}
	n := uint64(h[1] & 127)
	if n == 126 {
		var b [2]byte
		if _, e := io.ReadFull(r, b[:]); e != nil {
			return 0, nil, e
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	} else if n == 127 {
		var b [8]byte
		if _, e := io.ReadFull(r, b[:]); e != nil {
			return 0, nil, e
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxFrameBytes {
		return 0, nil, ErrTooLarge
	}
	var mask [4]byte
	if _, e := io.ReadFull(r, mask[:]); e != nil {
		return 0, nil, e
	}
	data := make([]byte, int(n))
	if _, e := io.ReadFull(r, data); e != nil {
		return 0, nil, e
	}
	for i := range data {
		data[i] ^= mask[i%4]
	}
	return h[0] & 15, data, nil
}
func TestPartialFrameIO(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 65535, 65536} {
		body := bytes.Repeat([]byte("x"), size)
		var out tinyWriter
		w := websocket{r: bufio.NewReader(tinyReader{bytes.NewReader(serverFrame(1, true, body))}), w: &out}
		got, err := w.readMessage()
		if err != nil || !bytes.Equal(body, got) {
			t.Fatal("partial read failed", size)
		}
		if w.writeFrame(1, body) != nil {
			t.Fatal("partial write failed")
		}
		op, got, err := readClientFrame(bytes.NewReader(out.Bytes()))
		if err != nil || op != 1 || !bytes.Equal(got, body) {
			t.Fatal("masked partial write corrupt", size)
		}
	}
	w := websocket{w: zeroWriter{}}
	if !errors.Is(w.writeFrame(1, []byte("x")), io.ErrShortWrite) {
		t.Fatal("zero write accepted")
	}
}
func TestMalformedFrames(t *testing.T) {
	cases := map[string][]byte{
		"masked-server": {0x81, 0x80, 0, 0, 0, 0}, "rsv": {0xc1, 0}, "binary": {0x82, 0}, "reserved-opcode": {0x83, 0}, "orphan-continuation": {0x80, 0}, "fragmented-ping": {0x09, 0}, "large-control": {0x89, 126, 0, 126}, "noncanonical16": {0x81, 126, 0, 1, 'a'}, "noncanonical64": {0x81, 127, 0, 0, 0, 0, 0, 0, 0, 1, 'a'}, "negative-length": {0x81, 127, 128, 0, 0, 0, 0, 0, 0, 0}, "truncated": {0x81, 3, 'a'}, "utf8": {0x81, 1, 0xff}, "nested-text": {0x01, 0, 0x81, 0}, "one-byte-close": {0x88, 1, 1}, "reserved-close": {0x88, 2, 3, 237}, "bad-close-utf8": {0x88, 3, 3, 232, 0xff},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			w := websocket{r: bufio.NewReader(bytes.NewReader(data)), w: io.Discard}
			if _, err := w.readMessage(); err == nil {
				t.Fatal("malformed frame accepted")
			}
		})
	}
}
func TestFrameAndAssembledCaps(t *testing.T) {
	oversized := append([]byte{0x81, 127}, binary.BigEndian.AppendUint64(nil, maxFrameBytes+1)...)
	w := websocket{r: bufio.NewReader(bytes.NewReader(oversized)), w: io.Discard}
	if _, err := w.readMessage(); !errors.Is(err, ErrTooLarge) {
		t.Fatal("frame cap missing")
	}
	part := bytes.Repeat([]byte("x"), maxMessageBytes/2+1)
	data := append(serverFrame(1, false, part), serverFrame(0, true, part)...)
	w.r = bufio.NewReader(bytes.NewReader(data))
	if _, err := w.readMessage(); !errors.Is(err, ErrTooLarge) {
		t.Fatal("fragment total cap missing")
	}
}
func TestCloseReply(t *testing.T) {
	var out bytes.Buffer
	w := websocket{r: bufio.NewReader(bytes.NewReader(serverFrame(8, true, []byte{3, 232}))), w: &out}
	if _, err := w.readMessage(); !errors.Is(err, ErrClosed) {
		t.Fatal("close not recognized")
	}
	op, body, err := readClientFrame(&out)
	if err != nil || op != 8 || !bytes.Equal(body, []byte{3, 232}) {
		t.Fatal("close reply missing")
	}
}
func validUpgrade(key string) []byte {
	return []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: WebSocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n")
}
func TestUpgradeValidationAndBufferedFrame(t *testing.T) {
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	if acceptKey(key) != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("RFC accept vector")
	}
	cases := map[string][]byte{
		"valid": validUpgrade(key), "bad-accept": bytes.ReplaceAll(validUpgrade(key), []byte(acceptKey(key)), []byte("wrong")), "no-upgrade": bytes.ReplaceAll(validUpgrade(key), []byte("keep-alive, Upgrade"), []byte("keep-alive")), "bad-status": bytes.ReplaceAll(validUpgrade(key), []byte("101 Switching Protocols"), []byte("200 OK")), "extensions": bytes.Replace(validUpgrade(key), []byte("\r\n\r\n"), []byte("\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n"), 1), "duplicate-accept": bytes.Replace(validUpgrade(key), []byte("\r\n\r\n"), []byte("\r\nSec-WebSocket-Accept: "+acceptKey(key)+"\r\n\r\n"), 1), "long-header": []byte("HTTP/1.1 101 Switching Protocols\r\nX: " + strings.Repeat("x", maxHeaderBytes) + "\r\n\r\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			err := readUpgrade(bufio.NewReader(tinyReader{bytes.NewReader(data)}), key)
			if (err == nil) != (name == "valid") {
				t.Fatal("upgrade validation failed")
			}
		})
	}
	r := bufio.NewReader(bytes.NewReader(append(validUpgrade(key), serverFrame(1, true, []byte("{}"))...)))
	if readUpgrade(r, key) != nil {
		t.Fatal("upgrade")
	}
	w := websocket{r: r, w: io.Discard}
	body, err := w.readMessage()
	if err != nil || string(body) != "{}" {
		t.Fatal("buffered frame lost")
	}
}
func FuzzWebSocketMessage(f *testing.F) {
	for _, seed := range [][]byte{serverFrame(1, true, []byte("{}")), {0x01, 1, 'a', 0x89, 0, 0x80, 1, 'b'}, {0x88, 2, 3, 232}, {0x81, 0x80}, {0x81, 127, 128, 0, 0, 0, 0, 0, 0, 0}, {0x80, 0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxMessageBytes {
			return
		}
		w := websocket{r: bufio.NewReader(bytes.NewReader(data)), w: io.Discard}
		got, err := w.readMessage()
		if err == nil && (len(got) > maxMessageBytes || !utf8.Valid(got)) {
			t.Fatal("invalid assembled message")
		}
	})
}
func FuzzUpgradeHeader(f *testing.F) {
	for _, seed := range [][]byte{validUpgrade("fixture"), []byte("HTTP/1.1 101\r\n\r\n"), []byte("HTTP/1.1 200 OK\r\n\r\n")} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) { _ = readUpgrade(bufio.NewReader(bytes.NewReader(data)), "fixture") })
}
