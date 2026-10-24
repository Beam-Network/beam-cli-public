package datasale

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// batchLeaves is how many leaves one unit of parallel work covers: 1 MiB of data.
const batchLeaves = 256

// MaxMismatches caps how many mismatching leaves Decrypt records; one is enough to challenge.
const MaxMismatches = 1000

// ErrSizeMismatch reports input that is shorter or longer than the sale says.
var ErrSizeMismatch = errors.New("input size does not match the sale")

type batch struct {
	seq    uint64
	first  uint64
	count  int
	data   []byte        // count leaves, each LeafBytes; the last leaf of a file is zero-padded
	hashes []common.Hash // per leaf: an input hash list entry, or a computed hash
	nodes  []common.Hash // per leaf: the tree leaf
	bad    []uint64
}

type pipeline struct {
	leaves uint64
	read   func(b *batch) error // fills data (and hashes, when a hash list is an input) for b.first..b.first+b.count
	work   func(b *batch)
	emit   func(b *batch) error
}

// run reads leaves in batches, processes them on every core, and emits results in index order.
func (p pipeline) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := runtime.NumCPU()
	todo := make(chan *batch, workers)
	done := make(chan *batch, workers)
	slots := make(chan struct{}, 2*workers)
	var readErr error
	go func() {
		defer close(todo)
		for seq, first := uint64(0), uint64(0); first < p.leaves; seq++ {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			count := uint64(batchLeaves)
			if p.leaves-first < count {
				count = p.leaves - first
			}
			b := &batch{seq: seq, first: first, count: int(count), data: make([]byte, int(count)*LeafBytes)}
			if err := p.read(b); err != nil {
				readErr = err
				cancel()
				return
			}
			select {
			case todo <- b:
			case <-ctx.Done():
				return
			}
			first += count
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range todo {
				p.work(b)
				select {
				case done <- b:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	pending := map[uint64]*batch{}
	next := uint64(0)
	var emitErr error
	for b := range done {
		if emitErr != nil {
			continue
		}
		pending[b.seq] = b
		for {
			ready, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if err := p.emit(ready); err != nil {
				emitErr = err
				cancel()
				break
			}
			<-slots
			next++
		}
	}
	if readErr != nil {
		return readErr
	}
	if emitErr != nil {
		return emitErr
	}
	return ctx.Err()
}

// readLeaves fills b.data from src, which holds total bytes; the bytes past total in the last leaf stay zero.
func readLeaves(src io.Reader, total uint64, b *batch) error {
	start := b.first * LeafBytes
	want := uint64(len(b.data))
	if start+want > total {
		want = total - start
	}
	if _, err := io.ReadFull(src, b.data[:want]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ErrSizeMismatch
		}
		return err
	}
	return nil
}

// expectEOF fails if src has bytes beyond what the sale covers.
func expectEOF(src io.Reader) error {
	var one [1]byte
	n, err := src.Read(one[:])
	if n > 0 {
		return ErrSizeMismatch
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// PlainResult is what the seller learns from its plaintext: the dataset fingerprint R_p and the leaf count.
type PlainResult struct {
	Root   common.Hash
	Leaves uint64
}

// BuildPlain reads a plaintext of size bytes and writes the hash list (h_0 … h_{n-1}, 32 bytes each) the buyer
// receives, and the plaintext tree into tree. It returns R_p.
func BuildPlain(ctx context.Context, src io.Reader, size uint64, hashList io.Writer, tree *TreeWriter) (_ PlainResult, err error) {
	defer tree.abandonOnError(&err)
	if size == 0 {
		return PlainResult{}, ErrSizeMismatch
	}
	leaves := LeafCount(size)
	err = pipeline{
		leaves: leaves,
		read:   func(b *batch) error { return readLeaves(src, size, b) },
		work: func(b *batch) {
			b.hashes = make([]common.Hash, b.count)
			b.nodes = make([]common.Hash, b.count)
			for k := 0; k < b.count; k++ {
				b.hashes[k] = crypto.Keccak256Hash(b.data[k*LeafBytes : (k+1)*LeafBytes])
				b.nodes[k] = PlainLeafHash(b.first+uint64(k), b.hashes[k])
			}
		},
		emit: func(b *batch) error {
			for k := 0; k < b.count; k++ {
				if _, err := hashList.Write(b.hashes[k][:]); err != nil {
					return err
				}
				if err := tree.Add(b.nodes[k]); err != nil {
					return err
				}
			}
			return nil
		},
	}.run(ctx)
	if err != nil {
		return PlainResult{}, err
	}
	if err := expectEOF(src); err != nil {
		return PlainResult{}, err
	}
	finished, err := tree.Finish()
	if err != nil {
		return PlainResult{}, err
	}
	root, err := finished.Root()
	return PlainResult{Root: root, Leaves: leaves}, err
}

// Encrypt reads a plaintext of size bytes, writes the ciphertext E (n·L bytes) the buyer receives, and builds the
// ciphertext tree for this sale. It returns R_c.
func Encrypt(ctx context.Context, src io.Reader, size uint64, saleID, key common.Hash, dst io.Writer, tree *TreeWriter) (common.Hash, error) {
	if size == 0 {
		return common.Hash{}, ErrSizeMismatch
	}
	err := pipeline{
		leaves: LeafCount(size),
		read:   func(b *batch) error { return readLeaves(src, size, b) },
		work: func(b *batch) {
			b.nodes = make([]common.Hash, b.count)
			for k := 0; k < b.count; k++ {
				leaf := b.data[k*LeafBytes : (k+1)*LeafBytes]
				XORKeystream(key, b.first+uint64(k), leaf, leaf)
				b.nodes[k] = CipherLeafHash(saleID, b.first+uint64(k), leaf)
			}
		},
		emit: func(b *batch) error {
			if _, err := dst.Write(b.data); err != nil {
				return err
			}
			for _, node := range b.nodes {
				if err := tree.Add(node); err != nil {
					return err
				}
			}
			return nil
		},
	}.run(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	if err := expectEOF(src); err != nil {
		return common.Hash{}, err
	}
	return finishRoot(tree)
}

// CipherTree builds the ciphertext tree over a received ciphertext of exactly leaves·L bytes. The buyer compares its
// root with the signed R_c before funding.
func CipherTree(ctx context.Context, src io.Reader, leaves uint64, saleID common.Hash, tree *TreeWriter) (_ common.Hash, err error) {
	defer tree.abandonOnError(&err)
	err = pipeline{
		leaves: leaves,
		read:   func(b *batch) error { return readLeaves(src, leaves*LeafBytes, b) },
		work: func(b *batch) {
			b.nodes = make([]common.Hash, b.count)
			for k := 0; k < b.count; k++ {
				b.nodes[k] = CipherLeafHash(saleID, b.first+uint64(k), b.data[k*LeafBytes:(k+1)*LeafBytes])
			}
		},
		emit: func(b *batch) error {
			for _, node := range b.nodes {
				if err := tree.Add(node); err != nil {
					return err
				}
			}
			return nil
		},
	}.run(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	if err := expectEOF(src); err != nil {
		return common.Hash{}, err
	}
	return finishRoot(tree)
}

// PlainTreeFromHashes rebuilds the plaintext tree from a received hash list of exactly leaves entries. The buyer
// compares its root with the signed R_p before funding.
func PlainTreeFromHashes(ctx context.Context, hashList io.Reader, leaves uint64, tree *TreeWriter) (_ common.Hash, err error) {
	defer tree.abandonOnError(&err)
	err = pipeline{
		leaves: leaves,
		read:   func(b *batch) error { return readHashes(hashList, b) },
		work: func(b *batch) {
			b.nodes = make([]common.Hash, b.count)
			for k := 0; k < b.count; k++ {
				b.nodes[k] = PlainLeafHash(b.first+uint64(k), b.hashes[k])
			}
		},
		emit: func(b *batch) error {
			for _, node := range b.nodes {
				if err := tree.Add(node); err != nil {
					return err
				}
			}
			return nil
		},
	}.run(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	if err := expectEOF(hashList); err != nil {
		return common.Hash{}, err
	}
	return finishRoot(tree)
}

func readHashes(hashList io.Reader, b *batch) error {
	buf := make([]byte, 32*b.count)
	if _, err := io.ReadFull(hashList, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ErrSizeMismatch
		}
		return err
	}
	b.hashes = make([]common.Hash, b.count)
	for k := range b.hashes {
		copy(b.hashes[k][:], buf[32*k:])
	}
	return nil
}

// DecryptResult reports leaves whose decryption does not hash to the committed value: the first MaxMismatches of
// them, and how many there were in all.
type DecryptResult struct {
	Mismatches     []uint64
	MismatchCount  uint64
	BytesDecrypted uint64
}

// Decrypt decrypts a ciphertext of LeafCount(size)·L bytes with the revealed key, checks every leaf against the hash
// list, and writes the plaintext, trimmed to size bytes, to dst.
func Decrypt(ctx context.Context, src io.Reader, size uint64, key common.Hash, hashList io.Reader, dst io.Writer) (DecryptResult, error) {
	var result DecryptResult
	leaves := LeafCount(size)
	err := pipeline{
		leaves: leaves,
		read: func(b *batch) error {
			if err := readLeaves(src, leaves*LeafBytes, b); err != nil {
				return err
			}
			return readHashes(hashList, b)
		},
		work: func(b *batch) {
			for k := 0; k < b.count; k++ {
				leaf := b.data[k*LeafBytes : (k+1)*LeafBytes]
				XORKeystream(key, b.first+uint64(k), leaf, leaf)
				if crypto.Keccak256Hash(leaf) != b.hashes[k] {
					b.bad = append(b.bad, b.first+uint64(k))
				}
			}
		},
		emit: func(b *batch) error {
			out := b.data
			end := (b.first + uint64(b.count)) * LeafBytes
			if end > size {
				out = out[:uint64(len(out))-(end-size)]
			}
			if _, err := dst.Write(out); err != nil {
				return err
			}
			result.BytesDecrypted += uint64(len(out))
			result.MismatchCount += uint64(len(b.bad))
			for _, index := range b.bad {
				if len(result.Mismatches) < MaxMismatches {
					result.Mismatches = append(result.Mismatches, index)
				}
			}
			return nil
		},
	}.run(ctx)
	if err != nil {
		return result, err
	}
	if err := expectEOF(src); err != nil {
		return result, err
	}
	if err := expectEOF(hashList); err != nil {
		return result, err
	}
	return result, nil
}

func finishRoot(tree *TreeWriter) (common.Hash, error) {
	finished, err := tree.Finish()
	if err != nil {
		return common.Hash{}, err
	}
	return finished.Root()
}

// String renders a short summary for logs and errors.
func (r DecryptResult) String() string {
	return fmt.Sprintf("%d bytes decrypted, %d mismatching leaves", r.BytesDecrypted, r.MismatchCount)
}
