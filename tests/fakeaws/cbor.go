package fakeaws

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// The newest AWS SDKs negotiate the rpc-v2-cbor protocol for some services.
// The fixture answers the handful of shapes the probe grid needs with a small
// deterministic CBOR encoder so the same scenario covers those services.

const protocolCBOR = "cbor"

const cborContentType = "application/cbor"

// cborTime encodes as a CBOR epoch timestamp (tag 1), the shape the SDK's
// rpc-v2-cbor decoder expects for a timestamp member.
type cborTime struct{ time.Time }

func writeCBORBody(writer http.ResponseWriter, status int, value any) {
	var buffer bytes.Buffer
	if err := encodeCBOR(&buffer, value); err != nil {
		writeBody(writer, "text/plain", http.StatusInternalServerError, "fixture encode error")
		return
	}
	writeBody(writer, cborContentType, status, buffer.String())
}

func writeCBORError(writer http.ResponseWriter, status int, code, message string) {
	writeCBORBody(writer, status, map[string]any{"__type": code, "message": message})
}

// encodeCBOR writes the subset of CBOR the fixture uses: unsigned integers,
// text strings, arrays, maps, booleans, and null.
func encodeCBOR(writer io.Writer, value any) error {
	switch typed := value.(type) {
	case nil:
		_, err := writer.Write([]byte{0xf6})
		return err
	case bool:
		if typed {
			_, err := writer.Write([]byte{0xf5})
			return err
		}
		_, err := writer.Write([]byte{0xf4})
		return err
	case cborTime:
		if err := writeCBORHead(writer, 6, 1); err != nil {
			return err
		}
		seconds := typed.Unix()
		if seconds < 0 {
			return fmt.Errorf("negative CBOR timestamp")
		}
		return writeCBORHead(writer, 0, uint64(seconds))
	case float64:
		var head [9]byte
		head[0] = 0xfb
		binary.BigEndian.PutUint64(head[1:], math.Float64bits(typed))
		_, err := writer.Write(head[:])
		return err
	case int:
		if typed < 0 {
			return fmt.Errorf("negative CBOR integer")
		}
		return writeCBORHead(writer, 0, uint64(typed))
	case string:
		if err := writeCBORHead(writer, 3, uint64(len(typed))); err != nil {
			return err
		}
		_, err := io.WriteString(writer, typed)
		return err
	case []any:
		if err := writeCBORHead(writer, 4, uint64(len(typed))); err != nil {
			return err
		}
		for _, item := range typed {
			if err := encodeCBOR(writer, item); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if err := writeCBORHead(writer, 5, uint64(len(typed))); err != nil {
			return err
		}
		for key, item := range typed {
			if err := encodeCBOR(writer, key); err != nil {
				return err
			}
			if err := encodeCBOR(writer, item); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported CBOR value")
	}
}

func writeCBORHead(writer io.Writer, major byte, length uint64) error {
	head := major << 5
	switch {
	case length < 24:
		_, err := writer.Write([]byte{head | byte(length)})
		return err
	case length <= 0xff:
		_, err := writer.Write([]byte{head | 24, byte(length)})
		return err
	case length <= 0xffff:
		_, err := writer.Write([]byte{head | 25, byte(length >> 8), byte(length)})
		return err
	case length <= 0xffffffff:
		_, err := writer.Write([]byte{head | 26, byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)})
		return err
	default:
		_, err := writer.Write([]byte{head | 27, byte(length >> 56), byte(length >> 48), byte(length >> 40), byte(length >> 32), byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)})
		return err
	}
}
