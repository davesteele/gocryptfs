// Package contentenc encrypts and decrypts file blocks.
package contentenc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"runtime"
	"sync"

	"github.com/hanwen/go-fuse/fuse"

	"github.com/rfjakob/gocryptfs/internal/cryptocore"
	"github.com/rfjakob/gocryptfs/internal/stupidgcm"
	"github.com/rfjakob/gocryptfs/internal/tlog"
)

// NonceMode determines how nonces are created.
type NonceMode int

const (
	// DefaultBS is the default plaintext block size
	DefaultBS = 4096
	// DefaultIVBits is the default length of IV, in bits.
	// We always use 128-bit IVs for file content, but the
	// master key in the config file is encrypted with a 96-bit IV for
	// gocryptfs v1.2 and earlier. v1.3 switched to 128 bit.
	DefaultIVBits = 128

	_ = iota // skip zero
	// RandomNonce chooses a random nonce.
	RandomNonce NonceMode = iota
	// ReverseDeterministicNonce chooses a deterministic nonce, suitable for
	// use in reverse mode.
	ReverseDeterministicNonce NonceMode = iota
	// ExternalNonce derives a nonce from external sources.
	ExternalNonce NonceMode = iota
)

// ContentEnc is used to encipher and decipher file content.
type ContentEnc struct {
	// Cryptographic primitives
	cryptoCore *cryptocore.CryptoCore
	// Plaintext block size
	plainBS uint64
	// Ciphertext block size
	cipherBS uint64
	// All-zero block of size cipherBS, for fast compares
	allZeroBlock []byte
	// All-zero block of size IVBitLen/8, for fast compares
	allZeroNonce []byte
	// Force decode even if integrity check fails (openSSL only)
	forceDecode bool
	// Ciphertext block pool. Always returns cipherBS-sized byte slices.
	cBlockPool bPool
	// Ciphertext request data pool. Always returns byte slices of size
	// fuse.MAX_KERNEL_WRITE + overhead.
	CReqPool bPool
	// Plaintext block pool. Always returns plainBS-sized byte slices.
	pBlockPool bPool
	// Plaintext request data pool. Slice have size fuse.MAX_KERNEL_WRITE.
	PReqPool bPool
}

// New returns an initialized ContentEnc instance.
func New(cc *cryptocore.CryptoCore, plainBS uint64, forceDecode bool) *ContentEnc {
	cipherBS := plainBS + uint64(cc.IVLen) + cryptocore.AuthTagLen
	// Take IV and GHASH overhead into account.
	cReqSize := int(fuse.MAX_KERNEL_WRITE / plainBS * cipherBS)
	if fuse.MAX_KERNEL_WRITE%plainBS != 0 {
		log.Panicf("unaligned MAX_KERNEL_WRITE=%d", fuse.MAX_KERNEL_WRITE)
	}
	c := &ContentEnc{
		cryptoCore:   cc,
		plainBS:      plainBS,
		cipherBS:     cipherBS,
		allZeroBlock: make([]byte, cipherBS),
		allZeroNonce: make([]byte, cc.IVLen),
		forceDecode:  forceDecode,
		cBlockPool:   newBPool(int(cipherBS)),
		CReqPool:     newBPool(cReqSize),
		pBlockPool:   newBPool(int(plainBS)),
		PReqPool:     newBPool(fuse.MAX_KERNEL_WRITE),
	}
	return c
}

// PlainBS returns the plaintext block size
func (be *ContentEnc) PlainBS() uint64 {
	return be.plainBS
}

// CipherBS returns the ciphertext block size
func (be *ContentEnc) CipherBS() uint64 {
	return be.cipherBS
}

// DecryptBlocks decrypts a number of blocks
func (be *ContentEnc) DecryptBlocks(ciphertext []byte, firstBlockNo uint64, fileID []byte) ([]byte, error) {
	cBuf := bytes.NewBuffer(ciphertext)
	var err error
	pBuf := bytes.NewBuffer(be.PReqPool.Get()[:0])
	for cBuf.Len() > 0 {
		cBlock := cBuf.Next(int(be.cipherBS))
		var pBlock []byte
		pBlock, err = be.DecryptBlock(cBlock, firstBlockNo, fileID)
		if err != nil {
			if be.forceDecode && err == stupidgcm.ErrAuth {
				tlog.Warn.Printf("DecryptBlocks: authentication failure in block #%d, overriden by forcedecode", firstBlockNo)
			} else {
				break
			}
		}
		pBuf.Write(pBlock)
		be.pBlockPool.Put(pBlock)
		firstBlockNo++
	}
	return pBuf.Bytes(), err
}

// DecryptBlock - Verify and decrypt GCM block
//
// Corner case: A full-sized block of all-zero ciphertext bytes is translated
// to an all-zero plaintext block, i.e. file hole passtrough.
func (be *ContentEnc) DecryptBlock(ciphertext []byte, blockNo uint64, fileID []byte) ([]byte, error) {

	// Empty block?
	if len(ciphertext) == 0 {
		return ciphertext, nil
	}

	// All-zero block?
	if bytes.Equal(ciphertext, be.allZeroBlock) {
		tlog.Debug.Printf("DecryptBlock: file hole encountered")
		return make([]byte, be.plainBS), nil
	}

	if len(ciphertext) < be.cryptoCore.IVLen {
		tlog.Warn.Printf("DecryptBlock: Block is too short: %d bytes", len(ciphertext))
		return nil, errors.New("Block is too short")
	}

	// Extract nonce
	nonce := ciphertext[:be.cryptoCore.IVLen]
	if bytes.Equal(nonce, be.allZeroNonce) {
		// Bug in tmpfs?
		// https://github.com/rfjakob/gocryptfs/issues/56
		// http://www.spinics.net/lists/kernel/msg2370127.html
		return nil, errors.New("all-zero nonce")
	}
	ciphertextOrig := ciphertext
	ciphertext = ciphertext[be.cryptoCore.IVLen:]

	// Decrypt
	plaintext := be.pBlockPool.Get()
	plaintext = plaintext[:0]
	aData := make([]byte, 8)
	aData = append(aData, fileID...)
	binary.BigEndian.PutUint64(aData, blockNo)
	plaintext, err := be.cryptoCore.AEADCipher.Open(plaintext, nonce, ciphertext, aData)

	if err != nil {
		tlog.Warn.Printf("DecryptBlock: %s, len=%d", err.Error(), len(ciphertextOrig))
		tlog.Debug.Println(hex.Dump(ciphertextOrig))
		if be.forceDecode && err == stupidgcm.ErrAuth {
			return plaintext, err
		}
		return nil, err
	}

	return plaintext, nil
}

// At some point, splitting the ciphertext into more groups will not improve
// performance, as spawning goroutines comes at a cost.
// 2 seems to work ok for now.
const encryptMaxSplit = 2

// EncryptBlocks is like EncryptBlock but takes multiple plaintext blocks.
func (be *ContentEnc) EncryptBlocks(plaintextBlocks [][]byte, firstBlockNo uint64, fileID []byte) []byte {
	ciphertextBlocks := make([][]byte, len(plaintextBlocks))
	// For large writes, we parallelize encryption.
	if len(plaintextBlocks) >= 32 {
		ncpu := runtime.NumCPU()
		if ncpu > encryptMaxSplit {
			ncpu = encryptMaxSplit
		}
		groupSize := len(plaintextBlocks) / ncpu
		var wg sync.WaitGroup
		for i := 0; i < ncpu; i++ {
			wg.Add(1)
			go func(i int) {
				low := i * groupSize
				high := (i + 1) * groupSize
				if i == ncpu-1 {
					// Last group, pick up any left-over blocks
					high = len(plaintextBlocks)
				}
				be.doEncryptBlocks(plaintextBlocks[low:high], ciphertextBlocks[low:high], firstBlockNo+uint64(low), fileID)
				wg.Done()
			}(i)
		}
		wg.Wait()
	} else {
		be.doEncryptBlocks(plaintextBlocks, ciphertextBlocks, firstBlockNo, fileID)
	}
	// Concatenate ciphertext into a single byte array.
	tmp := be.CReqPool.Get()
	out := bytes.NewBuffer(tmp[:0])
	for _, v := range ciphertextBlocks {
		out.Write(v)
		// Return the memory to cBlockPool
		be.cBlockPool.Put(v)
	}
	return out.Bytes()
}

// doEncryptBlocks is called by EncryptBlocks to do the actual encryption work
func (be *ContentEnc) doEncryptBlocks(in [][]byte, out [][]byte, firstBlockNo uint64, fileID []byte) {
	for i, v := range in {
		out[i] = be.EncryptBlock(v, firstBlockNo+uint64(i), fileID)
	}
}

// EncryptBlock - Encrypt plaintext using a random nonce.
// blockNo and fileID are used as associated data.
// The output is nonce + ciphertext + tag.
func (be *ContentEnc) EncryptBlock(plaintext []byte, blockNo uint64, fileID []byte) []byte {
	// Get a fresh random nonce
	nonce := be.cryptoCore.IVGenerator.Get()
	return be.doEncryptBlock(plaintext, blockNo, fileID, nonce)
}

// EncryptBlockNonce - Encrypt plaintext using a nonce chosen by the caller.
// blockNo and fileID are used as associated data.
// The output is nonce + ciphertext + tag.
// This function can only be used in SIV mode.
func (be *ContentEnc) EncryptBlockNonce(plaintext []byte, blockNo uint64, fileID []byte, nonce []byte) []byte {
	if be.cryptoCore.AEADBackend != cryptocore.BackendAESSIV {
		log.Panic("deterministic nonces are only secure in SIV mode")
	}
	return be.doEncryptBlock(plaintext, blockNo, fileID, nonce)
}

// doEncryptBlock is the backend for EncryptBlock and EncryptBlockNonce.
// blockNo and fileID are used as associated data.
// The output is nonce + ciphertext + tag.
func (be *ContentEnc) doEncryptBlock(plaintext []byte, blockNo uint64, fileID []byte, nonce []byte) []byte {
	// Empty block?
	if len(plaintext) == 0 {
		return plaintext
	}
	if len(nonce) != be.cryptoCore.IVLen {
		log.Panic("wrong nonce length")
	}
	// Block is authenticated with block number and file ID
	aData := make([]byte, 8)
	binary.BigEndian.PutUint64(aData, blockNo)
	aData = append(aData, fileID...)
	// Get a cipherBS-sized block of memory, copy the nonce into it and truncate to
	// nonce length
	cBlock := be.cBlockPool.Get()
	copy(cBlock, nonce)
	cBlock = cBlock[0:len(nonce)]
	// Encrypt plaintext and append to nonce
	ciphertext := be.cryptoCore.AEADCipher.Seal(cBlock, nonce, plaintext, aData)
	overhead := int(be.cipherBS - be.plainBS)
	if len(plaintext)+overhead != len(ciphertext) {
		log.Panicf("unexpected ciphertext length: plaintext=%d, overhead=%d, ciphertext=%d",
			len(plaintext), overhead, len(ciphertext))
	}
	return ciphertext
}

// MergeBlocks - Merge newData into oldData at offset
// New block may be bigger than both newData and oldData
func (be *ContentEnc) MergeBlocks(oldData []byte, newData []byte, offset int) []byte {
	// Fastpath for small-file creation
	if len(oldData) == 0 && offset == 0 {
		return newData
	}

	// Make block of maximum size
	out := make([]byte, be.plainBS)

	// Copy old and new data into it
	copy(out, oldData)
	l := len(newData)
	copy(out[offset:offset+l], newData)

	// Crop to length
	outLen := len(oldData)
	newLen := offset + len(newData)
	if outLen < newLen {
		outLen = newLen
	}
	return out[0:outLen]
}
