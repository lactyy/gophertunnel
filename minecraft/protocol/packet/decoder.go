package packet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"io"
)

// Decoder handles the decoding of Minecraft packets sent through an io.Reader. These packets in turn contain
// multiple compressed packets.
type Decoder struct {
	// r holds the io.Reader that packets are read from if the reader does not implement packetReader. When
	// this is the case, the buf field has a non-zero length.
	r   io.Reader
	buf []byte

	// pr holds a packetReader (and io.Reader) that packets are read from if the io.Reader passed to
	// NewDecoder implements the packetReader interface.
	pr packetReader

	decompress bool
	encrypt    *encrypt

	checkPacketLimit bool

	batched bool
}

// packetReader is used to read packets immediately instead of copying them in a buffer first. This is a
// specific case made to reduce RAM usage.
type packetReader interface {
	ReadPacket() ([]byte, error)
}

// NewDecoder returns a new decoder decoding data from the io.Reader passed. One read call from the reader is
// assumed to consume an entire packet.
func NewDecoder(reader io.Reader, batched bool) *Decoder {
	if pr, ok := reader.(packetReader); ok {
		return &Decoder{checkPacketLimit: true, pr: pr}
	}
	return &Decoder{
		r:                reader,
		buf:              make([]byte, 1024*1024*3),
		checkPacketLimit: true,
		batched:          batched,
	}
}

// EnableEncryption enables encryption for the Decoder using the secret key bytes passed. Each packet received
// will be decrypted.
func (decoder *Decoder) EnableEncryption(keyBytes [32]byte) {
	block, _ := aes.NewCipher(keyBytes[:])
	first12 := append([]byte(nil), keyBytes[:12]...)
	stream := cipher.NewCTR(block, append(first12, 0, 0, 0, 2))
	decoder.encrypt = newEncrypt(keyBytes[:], stream)
}

// EnableCompression enables compression for the Decoder.
func (decoder *Decoder) EnableCompression() {
	decoder.decompress = true
}

// DisableBatchPacketLimit disables the check that limits the number of packets allowed in a single packet
// batch. This should typically be called for Decoders decoding from a server connection.
func (decoder *Decoder) DisableBatchPacketLimit() {
	decoder.checkPacketLimit = false
}

const (
	// batchHeader is the batchHeader of compressed 'batches' from Minecraft.
	batchHeader = 0xfe
	// maximumInBatch is the maximum amount of packets that may be found in a batch. If a compressed batch has
	// more than this amount, decoding will fail.
	maximumInBatch = 812
)

// Decode decodes one 'packet' from the io.Reader passed in NewDecoder(), producing a slice of packets that it
// held and an error if not successful.
func (decoder *Decoder) Decode() (packets [][]byte, err error) {
	var data []byte
	if decoder.pr == nil {
		var n int
		n, err = decoder.r.Read(decoder.buf)
		data = decoder.buf[:n]
	} else {
		data, err = decoder.pr.ReadPacket()
	}
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if decoder.batched {
		if len(data) == 0 {
			return nil, nil
		}
		if data[0] != batchHeader {
			return nil, fmt.Errorf("decode batch: invalid batchHeader %x, expected %x", data[0], batchHeader)
		}
		data, err = decoder.decodePacket(data[1:])
		if err != nil {
			return nil, fmt.Errorf("decode batch: %w", err)
		}

		b := bytes.NewBuffer(data)
		for b.Len() != 0 {
			var length uint32
			if err := protocol.Varuint32(b, &length); err != nil {
				return nil, fmt.Errorf("decode batch: read packet length: %w", err)
			}
			packets = append(packets, b.Next(int(length)))
		}
		if len(packets) > maximumInBatch && decoder.checkPacketLimit {
			return nil, fmt.Errorf("decode batch: number of packets %v exceeds max=%v", len(packets), maximumInBatch)
		}
		return packets, nil
	} else {
		data, err = decoder.decodePacket(data)
		if err != nil {
			return nil, fmt.Errorf("decode single: %w", err)
		}

		b := bytes.NewBuffer(data)
		for b.Len() != 0 {
			var length uint32
			if err := protocol.Varuint32(b, &length); err != nil {
				return nil, fmt.Errorf("decode batch: read packet length: %w", err)
			}
			packets = append(packets, b.Next(int(length)))
		}
		if len(packets) > maximumInBatch && decoder.checkPacketLimit {
			return nil, fmt.Errorf("decode batch: number of packets %v exceeds max=%v", len(packets), maximumInBatch)
		}
		return packets, nil
		/*b := bytes.NewBuffer(data)
		var length uint32
		if err := protocol.Varuint32(b, &length); err != nil {
			return nil, fmt.Errorf("decode single: read packet single: %w", err)
		}
		return [][]byte{b.Next(int(length))}, nil*/
	}
}

func (decoder *Decoder) decodePacket(data []byte) (packet []byte, err error) {
	if decoder.encrypt != nil {
		decoder.encrypt.decrypt(data)
		if err := decoder.encrypt.verify(data); err != nil {
			// The packet did not have a correct checksum.
			return nil, fmt.Errorf("verify: %w", err)
		}
		data = data[:len(data)-8]
	}

	if decoder.decompress {
		if data[0] == 0xff {
			data = data[1:]
		} else {
			compression, ok := CompressionByID(uint16(data[0]))
			if !ok {
				return nil, fmt.Errorf("decompress: unknown compression algorithm %v", data[0])
			}
			data, err = compression.Decompress(data[1:])
			if err != nil {
				return nil, fmt.Errorf("decompress: %w", err)
			}
		}
	}
	return data, nil
}
