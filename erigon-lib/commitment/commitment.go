package commitment

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"math/bits"
	"strings"

	"github.com/ledgerwatch/log/v3"
	"golang.org/x/crypto/sha3"

	"github.com/ledgerwatch/erigon-lib/metrics"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/length"
	"github.com/ledgerwatch/erigon-lib/etl"
)

var (
	mxCommitmentKeys          = metrics.GetOrCreateCounter("domain_commitment_keys")
	mxCommitmentBranchUpdates = metrics.GetOrCreateCounter("domain_commitment_updates_applied")
)

// Trie represents commitment variant.
type Trie interface {
	// RootHash produces root hash of the trie
	RootHash() (hash []byte, err error)

	// Makes trie more verbose
	SetTrace(bool)

	// Variant returns commitment trie variant
	Variant() TrieVariant

	// Reset Drops everything from the trie
	Reset()

	// Set context for state IO
	ResetContext(ctx PatriciaContext)

	// Reads updates from storage
	ProcessKeys(ctx context.Context, pk [][]byte, logPrefix string) (rootHash []byte, err error)

	// Process already gathered updates
	ProcessUpdates(ctx context.Context, pk [][]byte, updates []Update) (rootHash []byte, err error)
}

type PatriciaContext interface {
	// GetBranch load branch node and fill up the cells
	// For each cell, it sets the cell type, clears the modified flag, fills the hash,
	// and for the extension, account, and leaf type, the `l` and `k`
	GetBranch(prefix []byte) ([]byte, uint64, error)
	// fetch account with given plain key
	GetAccount(plainKey []byte, cell *Cell) error
	// fetch storage with given plain key
	GetStorage(plainKey []byte, cell *Cell) error
	// Returns temp directory to use for update collecting
	TempDir() string
	// store branch data
	PutBranch(prefix []byte, data []byte, prevData []byte, prevStep uint64) error
}

type TrieVariant string

const (
	// VariantHexPatriciaTrie used as default commitment approach
	VariantHexPatriciaTrie TrieVariant = "hex-patricia-hashed"
	// VariantBinPatriciaTrie - Experimental mode with binary key representation
	VariantBinPatriciaTrie TrieVariant = "bin-patricia-hashed"
)

func InitializeTrie(tv TrieVariant) Trie {
	switch tv {
	case VariantBinPatriciaTrie:
		return NewBinPatriciaHashed(length.Addr, nil)
	case VariantHexPatriciaTrie:
		fallthrough
	default:
		return NewHexPatriciaHashed(length.Addr, nil)
	}
}

type PartFlags uint8

const (
	HashedKeyPart    PartFlags = 1
	AccountPlainPart PartFlags = 2
	StoragePlainPart PartFlags = 4
	HashPart         PartFlags = 8
)

type BranchData []byte

func (branchData BranchData) String() string {
	if len(branchData) == 0 {
		return ""
	}
	touchMap := binary.BigEndian.Uint16(branchData[0:])
	afterMap := binary.BigEndian.Uint16(branchData[2:])
	pos := 4
	var sb strings.Builder
	var cell Cell
	fmt.Fprintf(&sb, "touchMap %016b, afterMap %016b\n", touchMap, afterMap)
	for bitset, j := touchMap, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		fmt.Fprintf(&sb, "   %x => ", nibble)
		if afterMap&bit == 0 {
			sb.WriteString("{DELETED}\n")
		} else {
			fieldBits := PartFlags(branchData[pos])
			pos++
			var err error
			if pos, err = cell.fillFromFields(branchData, pos, fieldBits); err != nil {
				// This is used for test output, so ok to panic
				panic(err)
			}
			sb.WriteString("{")
			var comma string
			if cell.downHashedLen > 0 {
				fmt.Fprintf(&sb, "hashedKey=[%x]", cell.downHashedKey[:cell.downHashedLen])
				comma = ","
			}
			if cell.apl > 0 {
				fmt.Fprintf(&sb, "%saccountPlainKey=[%x]", comma, cell.apk[:cell.apl])
				comma = ","
			}
			if cell.spl > 0 {
				fmt.Fprintf(&sb, "%sstoragePlainKey=[%x]", comma, cell.spk[:cell.spl])
				comma = ","
			}
			if cell.hl > 0 {
				fmt.Fprintf(&sb, "%shash=[%x]", comma, cell.h[:cell.hl])
			}
			sb.WriteString("}\n")
		}
		bitset ^= bit
	}
	return sb.String()
}

type BranchEncoder struct {
	buf       *bytes.Buffer
	bitmapBuf [binary.MaxVarintLen64]byte
	updates   *etl.Collector
	tmpdir    string
}

func NewBranchEncoder(sz uint64, tmpdir string) *BranchEncoder {
	be := &BranchEncoder{
		buf:    bytes.NewBuffer(make([]byte, sz)),
		tmpdir: tmpdir,
	}
	be.initCollector()
	return be
}

func (be *BranchEncoder) initCollector() {
	be.updates = etl.NewCollector("commitment.BranchEncoder", be.tmpdir, etl.NewOldestEntryBuffer(etl.BufferOptimalSize/2), log.Root().New("branch-encoder"))
	be.updates.LogLvl(log.LvlDebug)
}

// reads previous comitted value and merges current with it if needed.
func loadToPatriciaContextFunc(pc PatriciaContext) etl.LoadFunc {
	return func(prefix, update []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
		stateValue, stateStep, err := pc.GetBranch(prefix)
		if err != nil {
			return err
		}
		// this updates ensures that if commitment is present, each branch are also present in commitment state at that moment with costs of storage
		//fmt.Printf("commitment branch encoder merge prefix [%x] [%x]->[%x]\n%v\n", prefix, stateValue, update, BranchData(update).String())

		cp, cu := common.Copy(prefix), common.Copy(update) // has to copy :(
		if err = pc.PutBranch(cp, cu, stateValue, stateStep); err != nil {
			return err
		}
		return nil
	}
}

func (be *BranchEncoder) Load(load etl.LoadFunc, args etl.TransformArgs) error {
	if err := be.updates.Load(nil, "", load, args); err != nil {
		return err
	}
	be.initCollector()
	return nil
}

func (be *BranchEncoder) CollectUpdate(
	prefix []byte,
	bitmap, touchMap, afterMap uint16,
	readCell func(nibble int, skip bool) (*Cell, error),
) (lastNibble int, err error) {

	v, ln, err := be.EncodeBranch(bitmap, touchMap, afterMap, readCell)
	if err != nil {
		return 0, err
	}
	//fmt.Printf("collectBranchUpdate [%x] -> [%x]\n", prefix, []byte(v))
	if err := be.updates.Collect(prefix, v); err != nil {
		return 0, err
	}
	return ln, nil
}

// Encoded result should be copied before next call to EncodeBranch, underlying slice is reused
func (be *BranchEncoder) EncodeBranch(bitmap, touchMap, afterMap uint16, readCell func(nibble int, skip bool) (*Cell, error)) (BranchData, int, error) {
	be.buf.Reset()

	if err := binary.Write(be.buf, binary.BigEndian, touchMap); err != nil {
		return nil, 0, err
	}
	if err := binary.Write(be.buf, binary.BigEndian, afterMap); err != nil {
		return nil, 0, err
	}

	putUvarAndVal := func(size uint64, val []byte) error {
		n := binary.PutUvarint(be.bitmapBuf[:], size)
		wn, err := be.buf.Write(be.bitmapBuf[:n])
		if err != nil {
			return err
		}
		if n != wn {
			return fmt.Errorf("n != wn size")
		}
		wn, err = be.buf.Write(val)
		if err != nil {
			return err
		}
		if len(val) != wn {
			return fmt.Errorf("wn != value size")
		}
		return nil
	}

	var lastNibble int
	for bitset, j := afterMap, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		for i := lastNibble; i < nibble; i++ {
			if _, err := readCell(i, true /* skip */); err != nil {
				return nil, 0, err
			} // only writes 0x80 into hasher
		}
		lastNibble = nibble + 1

		cell, err := readCell(nibble, false)
		if err != nil {
			return nil, 0, err
		}

		if bitmap&bit != 0 {
			var fieldBits PartFlags
			if cell.extLen > 0 && cell.spl == 0 {
				fieldBits |= HashedKeyPart
			}
			if cell.apl > 0 {
				fieldBits |= AccountPlainPart
			}
			if cell.spl > 0 {
				fieldBits |= StoragePlainPart
			}
			if cell.hl > 0 {
				fieldBits |= HashPart
			}
			if err := be.buf.WriteByte(byte(fieldBits)); err != nil {
				return nil, 0, err
			}
			if fieldBits&HashedKeyPart != 0 {
				if err := putUvarAndVal(uint64(cell.extLen), cell.extension[:cell.extLen]); err != nil {
					return nil, 0, err
				}
			}
			if fieldBits&AccountPlainPart != 0 {
				if err := putUvarAndVal(uint64(cell.apl), cell.apk[:cell.apl]); err != nil {
					return nil, 0, err
				}
			}
			if fieldBits&StoragePlainPart != 0 {
				if err := putUvarAndVal(uint64(cell.spl), cell.spk[:cell.spl]); err != nil {
					return nil, 0, err
				}
			}
			if fieldBits&HashPart != 0 {
				if err := putUvarAndVal(uint64(cell.hl), cell.h[:cell.hl]); err != nil {
					return nil, 0, err
				}
			}
		}
		bitset ^= bit
	}
	//fmt.Printf("EncodeBranch [%x] size: %d\n", be.buf.Bytes(), be.buf.Len())
	return be.buf.Bytes(), lastNibble, nil
}

func RetrieveCellNoop(nibble int, skip bool) (*Cell, error) { return nil, nil }

// if fn returns nil, the original key will be copied from branchData
func (branchData BranchData) ReplacePlainKeys(newData []byte, fn func(key []byte, isStorage bool) (newKey []byte, err error)) (BranchData, error) {
	if len(branchData) < 4 {
		return branchData, nil
	}

	var numBuf [binary.MaxVarintLen64]byte
	touchMap := binary.BigEndian.Uint16(branchData[0:])
	afterMap := binary.BigEndian.Uint16(branchData[2:])
	if touchMap&afterMap == 0 {
		return branchData, nil
	}
	pos := 4
	newData = append(newData[:0], branchData[:4]...)
	for bitset, j := touchMap&afterMap, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		fieldBits := PartFlags(branchData[pos])
		newData = append(newData, byte(fieldBits))
		pos++
		if fieldBits&HashedKeyPart != 0 {
			l, n := binary.Uvarint(branchData[pos:])
			if n == 0 {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for hashedKey len")
			} else if n < 0 {
				return nil, fmt.Errorf("replacePlainKeys value overflow for hashedKey len")
			}
			newData = append(newData, branchData[pos:pos+n]...)
			pos += n
			if len(branchData) < pos+int(l) {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for hashedKey")
			}
			if l > 0 {
				newData = append(newData, branchData[pos:pos+int(l)]...)
				pos += int(l)
			}
		}
		if fieldBits&AccountPlainPart != 0 {
			l, n := binary.Uvarint(branchData[pos:])
			if n == 0 {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for accountPlainKey len")
			} else if n < 0 {
				return nil, fmt.Errorf("replacePlainKeys value overflow for accountPlainKey len")
			}
			pos += n
			if len(branchData) < pos+int(l) {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for accountPlainKey")
			}
			if l > 0 {
				pos += int(l)
			}
			newKey, err := fn(branchData[pos-int(l):pos], false)
			if err != nil {
				return nil, err
			}
			if newKey == nil {
				newData = append(newData, branchData[pos-int(l)-n:pos]...)
				if l != length.Addr {
					fmt.Printf("COPY %x LEN %d\n", []byte(branchData[pos-int(l):pos]), l)
				}
			} else {
				if len(newKey) > 8 && len(newKey) != length.Addr {
					fmt.Printf("SHORT %x LEN %d\n", newKey, len(newKey))
				}

				n = binary.PutUvarint(numBuf[:], uint64(len(newKey)))
				newData = append(newData, numBuf[:n]...)
				newData = append(newData, newKey...)
			}
		}
		if fieldBits&StoragePlainPart != 0 {
			l, n := binary.Uvarint(branchData[pos:])
			if n == 0 {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for storagePlainKey len")
			} else if n < 0 {
				return nil, fmt.Errorf("replacePlainKeys value overflow for storagePlainKey len")
			}
			pos += n
			if len(branchData) < pos+int(l) {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for storagePlainKey")
			}
			if l > 0 {
				pos += int(l)
			}
			newKey, err := fn(branchData[pos-int(l):pos], true)
			if err != nil {
				return nil, err
			}
			if newKey == nil {
				newData = append(newData, branchData[pos-int(l)-n:pos]...) // -n to include length
				if l != length.Addr+length.Hash {
					fmt.Printf("COPY %x LEN %d\n", []byte(branchData[pos-int(l):pos]), l)
				}
			} else {
				if len(newKey) > 8 && len(newKey) != length.Addr+length.Hash {
					fmt.Printf("SHORT %x LEN %d\n", newKey, len(newKey))
				}

				n = binary.PutUvarint(numBuf[:], uint64(len(newKey)))
				newData = append(newData, numBuf[:n]...)
				newData = append(newData, newKey...)
			}
		}
		if fieldBits&HashPart != 0 {
			l, n := binary.Uvarint(branchData[pos:])
			if n == 0 {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for hash len")
			} else if n < 0 {
				return nil, fmt.Errorf("replacePlainKeys value overflow for hash len")
			}
			newData = append(newData, branchData[pos:pos+n]...)
			pos += n
			if len(branchData) < pos+int(l) {
				return nil, fmt.Errorf("replacePlainKeys buffer too small for hash")
			}
			if l > 0 {
				newData = append(newData, branchData[pos:pos+int(l)]...)
				pos += int(l)
			}
		}
		bitset ^= bit
	}

	return newData, nil
}

// IsComplete determines whether given branch data is complete, meaning that all information about all the children is present
// Each of 16 children of a branch node have two attributes
// touch - whether this child has been modified or deleted in this branchData (corresponding bit in touchMap is set)
// after - whether after this branchData application, the child is present in the tree or not (corresponding bit in afterMap is set)
func (branchData BranchData) IsComplete() bool {
	touchMap := binary.BigEndian.Uint16(branchData[0:])
	afterMap := binary.BigEndian.Uint16(branchData[2:])
	return ^touchMap&afterMap == 0
}

// MergeHexBranches combines two branchData, number 2 coming after (and potentially shadowing) number 1
func (branchData BranchData) MergeHexBranches(branchData2 BranchData, newData []byte) (BranchData, error) {
	if branchData2 == nil {
		return branchData, nil
	}
	if branchData == nil {
		return branchData2, nil
	}

	touchMap1 := binary.BigEndian.Uint16(branchData[0:])
	afterMap1 := binary.BigEndian.Uint16(branchData[2:])
	bitmap1 := touchMap1 & afterMap1
	pos1 := 4
	touchMap2 := binary.BigEndian.Uint16(branchData2[0:])
	afterMap2 := binary.BigEndian.Uint16(branchData2[2:])
	bitmap2 := touchMap2 & afterMap2
	pos2 := 4
	var bitmapBuf [4]byte
	binary.BigEndian.PutUint16(bitmapBuf[0:], touchMap1|touchMap2)
	binary.BigEndian.PutUint16(bitmapBuf[2:], afterMap2)
	newData = append(newData, bitmapBuf[:]...)
	for bitset, j := bitmap1|bitmap2, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		if bitmap2&bit != 0 {
			// Add fields from branchData2
			fieldBits := PartFlags(branchData2[pos2])
			newData = append(newData, byte(fieldBits))
			pos2++
			for i := 0; i < bits.OnesCount8(byte(fieldBits)); i++ {
				l, n := binary.Uvarint(branchData2[pos2:])
				if n == 0 {
					return nil, fmt.Errorf("MergeHexBranches buffer2 too small for field")
				} else if n < 0 {
					return nil, fmt.Errorf("MergeHexBranches value2 overflow for field")
				}
				newData = append(newData, branchData2[pos2:pos2+n]...)
				pos2 += n
				if len(branchData2) < pos2+int(l) {
					return nil, fmt.Errorf("MergeHexBranches buffer2 too small for field")
				}
				if l > 0 {
					newData = append(newData, branchData2[pos2:pos2+int(l)]...)
					pos2 += int(l)
				}
			}
		}
		if bitmap1&bit != 0 {
			add := (touchMap2&bit == 0) && (afterMap2&bit != 0) // Add fields from branchData1
			fieldBits := PartFlags(branchData[pos1])
			if add {
				newData = append(newData, byte(fieldBits))
			}
			pos1++
			for i := 0; i < bits.OnesCount8(byte(fieldBits)); i++ {
				l, n := binary.Uvarint(branchData[pos1:])
				if n == 0 {
					return nil, fmt.Errorf("MergeHexBranches buffer1 too small for field")
				} else if n < 0 {
					return nil, fmt.Errorf("MergeHexBranches value1 overflow for field")
				}
				if add {
					newData = append(newData, branchData[pos1:pos1+n]...)
				}
				pos1 += n
				if len(branchData) < pos1+int(l) {
					return nil, fmt.Errorf("MergeHexBranches buffer1 too small for field")
				}
				if l > 0 {
					if add {
						newData = append(newData, branchData[pos1:pos1+int(l)]...)
					}
					pos1 += int(l)
				}
			}
		}
		bitset ^= bit
	}
	return newData, nil
}

func (branchData BranchData) DecodeCells() (touchMap, afterMap uint16, row [16]*Cell, err error) {
	touchMap = binary.BigEndian.Uint16(branchData[0:])
	afterMap = binary.BigEndian.Uint16(branchData[2:])
	pos := 4
	for bitset, j := touchMap, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		nibble := bits.TrailingZeros16(bit)
		if afterMap&bit != 0 {
			fieldBits := PartFlags(branchData[pos])
			pos++
			row[nibble] = new(Cell)
			if pos, err = row[nibble].fillFromFields(branchData, pos, fieldBits); err != nil {
				err = fmt.Errorf("faield to fill cell at nibble %x: %w", nibble, err)
				return
			}
		}
		bitset ^= bit
	}
	return
}

type BranchMerger struct {
	buf    *bytes.Buffer
	num    [4]byte
	keccak hash.Hash
}

func NewHexBranchMerger(capacity uint64) *BranchMerger {
	return &BranchMerger{buf: bytes.NewBuffer(make([]byte, capacity)), keccak: sha3.NewLegacyKeccak256()}
}

// MergeHexBranches combines two branchData, number 2 coming after (and potentially shadowing) number 1
func (m *BranchMerger) Merge(branch1 BranchData, branch2 BranchData) (BranchData, error) {
	if len(branch2) == 0 {
		return branch1, nil
	}
	if len(branch1) == 0 {
		return branch2, nil
	}

	touchMap1 := binary.BigEndian.Uint16(branch1[0:])
	afterMap1 := binary.BigEndian.Uint16(branch1[2:])
	bitmap1 := touchMap1 & afterMap1
	pos1 := 4

	touchMap2 := binary.BigEndian.Uint16(branch2[0:])
	afterMap2 := binary.BigEndian.Uint16(branch2[2:])
	bitmap2 := touchMap2 & afterMap2
	pos2 := 4

	binary.BigEndian.PutUint16(m.num[0:], touchMap1|touchMap2)
	binary.BigEndian.PutUint16(m.num[2:], afterMap2)
	dataPos := 4

	m.buf.Reset()
	if _, err := m.buf.Write(m.num[:]); err != nil {
		return nil, err
	}

	for bitset, j := bitmap1|bitmap2, 0; bitset != 0; j++ {
		bit := bitset & -bitset
		if bitmap2&bit != 0 {
			// Add fields from branch2
			fieldBits := PartFlags(branch2[pos2])
			if err := m.buf.WriteByte(byte(fieldBits)); err != nil {
				return nil, err
			}
			pos2++

			for i := 0; i < bits.OnesCount8(byte(fieldBits)); i++ {
				l, n := binary.Uvarint(branch2[pos2:])
				if n == 0 {
					return nil, fmt.Errorf("MergeHexBranches branch2 is too small: expected node info size")
				} else if n < 0 {
					return nil, fmt.Errorf("MergeHexBranches branch2: size overflow for length")
				}

				_, err := m.buf.Write(branch2[pos2 : pos2+n])
				if err != nil {
					return nil, err
				}
				pos2 += n
				dataPos += n
				if len(branch2) < pos2+int(l) {
					return nil, fmt.Errorf("MergeHexBranches branch2 is too small: expected at least %d got %d bytes", pos2+int(l), len(branch2))
				}
				if l > 0 {
					if _, err := m.buf.Write(branch2[pos2 : pos2+int(l)]); err != nil {
						return nil, err
					}
					pos2 += int(l)
					dataPos += int(l)
				}
			}
		}
		if bitmap1&bit != 0 {
			add := (touchMap2&bit == 0) && (afterMap2&bit != 0) // Add fields from branchData1
			fieldBits := PartFlags(branch1[pos1])
			if add {
				if err := m.buf.WriteByte(byte(fieldBits)); err != nil {
					return nil, err
				}
			}
			pos1++
			for i := 0; i < bits.OnesCount8(byte(fieldBits)); i++ {
				l, n := binary.Uvarint(branch1[pos1:])
				if n == 0 {
					return nil, fmt.Errorf("MergeHexBranches branch1 is too small: expected node info size")
				} else if n < 0 {
					return nil, fmt.Errorf("MergeHexBranches branch1: size overflow for length")
				}
				if add {
					if _, err := m.buf.Write(branch1[pos1 : pos1+n]); err != nil {
						return nil, err
					}
				}
				pos1 += n
				if len(branch1) < pos1+int(l) {
					fmt.Printf("b1: %x %v\n", branch1, branch1)
					fmt.Printf("b2: %x\n", branch2)
					return nil, fmt.Errorf("MergeHexBranches branch1 is too small: expected at least %d got %d bytes", pos1+int(l), len(branch1))
				}
				if l > 0 {
					if add {
						if _, err := m.buf.Write(branch1[pos1 : pos1+int(l)]); err != nil {
							return nil, err
						}
					}
					pos1 += int(l)
				}
			}
		}
		bitset ^= bit
	}
	target := make([]byte, m.buf.Len())
	copy(target, m.buf.Bytes())
	return target, nil
}

func ParseTrieVariant(s string) TrieVariant {
	var trieVariant TrieVariant
	switch s {
	case "bin":
		trieVariant = VariantBinPatriciaTrie
	case "hex":
		fallthrough
	default:
		trieVariant = VariantHexPatriciaTrie
	}
	return trieVariant
}

type BranchStat struct {
	KeySize     uint64
	ValSize     uint64
	MinCellSize uint64
	MaxCellSize uint64
	CellCount   uint64
	APKSize     uint64
	SPKSize     uint64
	ExtSize     uint64
	HashSize    uint64
	APKCount    uint64
	SPKCount    uint64
	HashCount   uint64
	ExtCount    uint64
	TAMapsSize  uint64
	IsRoot      bool
}

// do not add stat of root node to other branch stat
func (bs *BranchStat) Collect(other *BranchStat) {
	if other == nil {
		return
	}

	bs.KeySize += other.KeySize
	bs.ValSize += other.ValSize
	bs.MinCellSize = min(bs.MinCellSize, other.MinCellSize)
	bs.MaxCellSize = max(bs.MaxCellSize, other.MaxCellSize)
	bs.CellCount += other.CellCount
	bs.APKSize += other.APKSize
	bs.SPKSize += other.SPKSize
	bs.ExtSize += other.ExtSize
	bs.HashSize += other.HashSize
	bs.APKCount += other.APKCount
	bs.SPKCount += other.SPKCount
	bs.HashCount += other.HashCount
	bs.ExtCount += other.ExtCount
}

func DecodeBranchAndCollectStat(key, branch []byte, tv TrieVariant) *BranchStat {
	stat := &BranchStat{}
	if len(key) == 0 {
		return nil
	}

	stat.KeySize = uint64(len(key))
	stat.ValSize = uint64(len(branch))
	stat.IsRoot = true

	// if key is not "state" then we are interested in the branch data
	if !bytes.Equal(key, []byte("state")) {
		stat.IsRoot = false

		tm, am, cells, err := BranchData(branch).DecodeCells()
		if err != nil {
			return nil
		}
		stat.TAMapsSize = uint64(2 + 2) // touchMap + afterMap
		stat.CellCount = uint64(bits.OnesCount16(tm & am))
		for _, c := range cells {
			if c == nil {
				continue
			}
			enc := uint64(len(c.Encode()))
			stat.MinCellSize = min(stat.MinCellSize, enc)
			stat.MaxCellSize = max(stat.MaxCellSize, enc)
			switch {
			case c.apl > 0:
				stat.APKSize += uint64(c.apl)
				stat.APKCount++
			case c.spl > 0:
				stat.SPKSize += uint64(c.spl)
				stat.SPKCount++
			case c.hl > 0:
				stat.HashSize += uint64(c.hl)
				stat.HashCount++
			default:
				panic("no plain key" + fmt.Sprintf("#+%v", c))
				//case c.extLen > 0:
			}
			if c.extLen > 0 {
				switch tv {
				case VariantBinPatriciaTrie:
					stat.ExtSize += uint64(c.extLen)
				case VariantHexPatriciaTrie:
					stat.ExtSize += uint64(c.extLen)
				}
				stat.ExtCount++
			}
		}
	}
	return stat
}
