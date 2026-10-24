package datasale

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Tree is a Merkle tree in the escrow's shape: level 0 is the leaves in index order, each next level pairs nodes
// (2m, 2m+1) into H(min ‖ max), and an odd last node is carried up unchanged. Each level is one file of 32-byte
// nodes in a directory, so a tree over a 100 GB file needs little memory and a proof reads only its siblings.
type Tree struct {
	dir    string
	counts []uint64
}

// ErrEmptyTree reports a tree with no leaves.
var ErrEmptyTree = errors.New("tree has no leaves")

func levelPath(dir string, level int) string {
	return filepath.Join(dir, fmt.Sprintf("level-%02d", level))
}

// TreeWriter receives the leaves of a new tree in index order.
type TreeWriter struct {
	dir   string
	file  *os.File
	w     *bufio.Writer
	count uint64
}

// NewTreeWriter starts a tree in dir, which must be empty or absent.
func NewTreeWriter(dir string) (*TreeWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(levelPath(dir, 0), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &TreeWriter{dir: dir, file: file, w: bufio.NewWriterSize(file, 1<<20)}, nil
}

// Add appends the next leaf.
func (t *TreeWriter) Add(leaf common.Hash) error {
	if _, err := t.w.Write(leaf[:]); err != nil {
		return err
	}
	t.count++
	return nil
}

// abandonOnError closes the leaf file when the build that fills the tree fails, so the caller can remove the tree's
// directory, which Windows refuses while a file in it is open.
func (t *TreeWriter) abandonOnError(err *error) {
	if *err != nil {
		_ = t.file.Close()
	}
}

// Finish builds every upper level and returns the tree.
func (t *TreeWriter) Finish() (*Tree, error) {
	if err := t.w.Flush(); err != nil {
		t.file.Close()
		return nil, err
	}
	if err := t.file.Close(); err != nil {
		return nil, err
	}
	if t.count == 0 {
		return nil, ErrEmptyTree
	}
	counts := []uint64{t.count}
	for level := 0; counts[level] > 1; level++ {
		next, err := buildLevel(t.dir, level, counts[level])
		if err != nil {
			return nil, err
		}
		counts = append(counts, next)
	}
	return &Tree{dir: t.dir, counts: counts}, nil
}

func buildLevel(dir string, level int, count uint64) (uint64, error) {
	in, err := os.Open(levelPath(dir, level))
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(levelPath(dir, level+1), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriterSize(out, 1<<20)
	var a, b common.Hash
	var written uint64
	for i := uint64(0); i < count; i += 2 {
		if _, err := io.ReadFull(r, a[:]); err != nil {
			out.Close()
			return 0, err
		}
		node := a
		if i+1 < count {
			if _, err := io.ReadFull(r, b[:]); err != nil {
				out.Close()
				return 0, err
			}
			node = hashPair(a, b)
		}
		if _, err := w.Write(node[:]); err != nil {
			out.Close()
			return 0, err
		}
		written++
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return 0, err
	}
	return written, out.Close()
}

func hashPair(a, b common.Hash) common.Hash {
	if bytes.Compare(a[:], b[:]) < 0 {
		return crypto.Keccak256Hash(a[:], b[:])
	}
	return crypto.Keccak256Hash(b[:], a[:])
}

// OpenTree opens a tree an earlier TreeWriter finished in dir.
func OpenTree(dir string) (*Tree, error) {
	var counts []uint64
	for level := 0; ; level++ {
		info, err := os.Stat(levelPath(dir, level))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, err
		}
		if info.Size()%32 != 0 {
			return nil, fmt.Errorf("tree level %d is not a whole number of nodes", level)
		}
		counts = append(counts, uint64(info.Size()/32))
	}
	if len(counts) == 0 || counts[0] == 0 {
		return nil, ErrEmptyTree
	}
	if counts[len(counts)-1] != 1 {
		return nil, fmt.Errorf("tree in %s is incomplete", dir)
	}
	return &Tree{dir: dir, counts: counts}, nil
}

// Leaves is the number of leaves.
func (t *Tree) Leaves() uint64 { return t.counts[0] }

// Root is the single node of the top level.
func (t *Tree) Root() (common.Hash, error) {
	return t.node(len(t.counts)-1, 0)
}

// Leaf returns leaf index as stored.
func (t *Tree) Leaf(index uint64) (common.Hash, error) {
	if index >= t.counts[0] {
		return common.Hash{}, fmt.Errorf("leaf %d is out of range", index)
	}
	return t.node(0, index)
}

// Proof lists the siblings met on the way from leaf index to the root, skipping levels where the node is carried up,
// as OpenZeppelin's MerkleProof expects.
func (t *Tree) Proof(index uint64) ([]common.Hash, error) {
	if index >= t.counts[0] {
		return nil, fmt.Errorf("leaf %d is out of range", index)
	}
	var proof []common.Hash
	for level := 0; level < len(t.counts)-1; level++ {
		sibling := index ^ 1
		if sibling < t.counts[level] {
			node, err := t.node(level, sibling)
			if err != nil {
				return nil, err
			}
			proof = append(proof, node)
		}
		index /= 2
	}
	return proof, nil
}

func (t *Tree) node(level int, index uint64) (common.Hash, error) {
	f, err := os.Open(levelPath(t.dir, level))
	if err != nil {
		return common.Hash{}, err
	}
	defer f.Close()
	var h common.Hash
	if _, err := f.ReadAt(h[:], int64(index)*32); err != nil {
		return common.Hash{}, err
	}
	return h, nil
}

// ProcessProof hashes leaf up through proof, as OpenZeppelin's MerkleProof.processProof does.
func ProcessProof(proof []common.Hash, leaf common.Hash) common.Hash {
	node := leaf
	for _, sibling := range proof {
		node = hashPair(node, sibling)
	}
	return node
}
