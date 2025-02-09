package commitment

import (
	"encoding/binary"
	"encoding/hex"
	"math/rand"
	"testing"

	"github.com/ledgerwatch/erigon-lib/common"

	"github.com/stretchr/testify/require"
)

func generateCellRow(t *testing.T, size int) (row []*Cell, bitmap uint16) {
	t.Helper()

	row = make([]*Cell, size)
	var bm uint16
	for i := 0; i < len(row); i++ {
		row[i] = new(Cell)
		row[i].hl = 32
		n, err := rand.Read(row[i].h[:])
		require.NoError(t, err)
		require.EqualValues(t, row[i].hl, n)

		th := rand.Intn(120)
		switch {
		case th > 70:
			n, err = rand.Read(row[i].apk[:])
			require.NoError(t, err)
			row[i].apl = n
		case th > 20 && th <= 70:
			n, err = rand.Read(row[i].spk[:])
			require.NoError(t, err)
			row[i].spl = n
		case th <= 20:
			n, err = rand.Read(row[i].extension[:th])
			row[i].extLen = n
			require.NoError(t, err)
			require.EqualValues(t, th, n)
		}
		bm |= uint16(1 << i)
	}
	return row, bm
}

func TestBranchData_MergeHexBranches2(t *testing.T) {
	row, bm := generateCellRow(t, 16)

	be := NewBranchEncoder(1024, t.TempDir())
	enc, _, err := be.EncodeBranch(bm, bm, bm, func(i int, skip bool) (*Cell, error) {
		return row[i], nil
	})

	require.NoError(t, err)
	require.NotEmpty(t, enc)
	t.Logf("enc [%d] %x\n", len(enc), enc)

	bmg := NewHexBranchMerger(8192)
	res, err := bmg.Merge(enc, enc)
	require.NoError(t, err)
	require.EqualValues(t, enc, res)

	tm, am, origins, err := res.DecodeCells()
	require.NoError(t, err)
	require.EqualValues(t, tm, am)
	require.EqualValues(t, bm, am)

	i := 0
	for _, c := range origins {
		if c == nil {
			continue
		}
		require.EqualValues(t, row[i].extLen, c.extLen)
		require.EqualValues(t, row[i].extension, c.extension)
		require.EqualValues(t, row[i].apl, c.apl)
		require.EqualValues(t, row[i].apk, c.apk)
		require.EqualValues(t, row[i].spl, c.spl)
		require.EqualValues(t, row[i].spk, c.spk)
		i++
	}
}

func TestBranchData_MergeHexBranchesEmptyBranches(t *testing.T) {
	// Create a BranchMerger instance with sufficient capacity for testing.
	merger := NewHexBranchMerger(1024)

	// Test merging when one branch is empty.
	branch1 := BranchData{}
	branch2 := BranchData{0x02, 0x02, 0x03, 0x03, 0x0C, 0x02, 0x04, 0x0C}
	mergedBranch, err := merger.Merge(branch1, branch2)
	require.NoError(t, err)
	require.Equal(t, branch2, mergedBranch)

	// Test merging when both branches are empty.
	branch1 = BranchData{}
	branch2 = BranchData{}
	mergedBranch, err = merger.Merge(branch1, branch2)
	require.NoError(t, err)
	require.Equal(t, branch1, mergedBranch)
}

// Additional tests for error cases, edge cases, and other scenarios can be added here.

func TestBranchData_MergeHexBranches3(t *testing.T) {
	encs := "0405040b04080f0b080d030204050b0502090805050d01060e060d070f0903090c04070a0d0a000e090b060b0c040c0700020e0b0c060b0106020c0607050a0b0209070d06040808"
	enc, err := hex.DecodeString(encs)
	require.NoError(t, err)

	//tm, am, origins, err := BranchData(enc).DecodeCells()
	require.NoError(t, err)
	t.Logf("%s", BranchData(enc).String())
	//require.EqualValues(t, tm, am)
	//_, _ = tm, am
}

// helper to decode row of cells from string
func unfoldBranchDataFromString(t *testing.T, encs string) (row []*Cell, am uint16) {
	t.Helper()

	//encs := "0405040b04080f0b080d030204050b0502090805050d01060e060d070f0903090c04070a0d0a000e090b060b0c040c0700020e0b0c060b0106020c0607050a0b0209070d06040808"
	//encs := "37ad10eb75ea0fc1c363db0dda0cd2250426ee2c72787155101ca0e50804349a94b649deadcc5cddc0d2fd9fb358c2edc4e7912d165f88877b1e48c69efacf418e923124506fbb2fd64823fd41cbc10427c423"
	enc, err := hex.DecodeString(encs)
	require.NoError(t, err)

	tm, am, origins, err := BranchData(enc).DecodeCells()
	require.NoError(t, err)
	_, _ = tm, am

	t.Logf("%s", BranchData(enc).String())
	//require.EqualValues(t, tm, am)
	//for i, c := range origins {
	//	if c == nil {
	//		continue
	//	}
	//	fmt.Printf("i %d, c %#+v\n", i, c)
	//}
	return origins[:], am
}

func TestBranchData_ReplacePlainKeys(t *testing.T) {
	row, bm := generateCellRow(t, 16)

	cells, am := unfoldBranchDataFromString(t, "86e586e5082035e72a782b51d9c98548467e3f868294d923cdbbdf4ce326c867bd972c4a2395090109203b51781a76dc87640aea038e3fdd8adca94049aaa436735b162881ec159f6fb408201aa2fa41b5fb019e8abf8fc32800805a2743cfa15373cf64ba16f4f70e683d8e0404a192d9050404f993d9050404e594d90508208642542ff3ce7d63b9703e85eb924ab3071aa39c25b1651c6dda4216387478f10404bd96d905")
	for i, c := range cells {
		if c == nil {
			continue
		}
		if c.apl > 0 {
			offt, _ := binary.Uvarint(c.apk[:c.apl])
			t.Logf("%d apk %x, offt %d\n", i, c.apk[:c.apl], offt)
		}
		if c.spl > 0 {
			offt, _ := binary.Uvarint(c.spk[:c.spl])
			t.Logf("%d spk %x offt %d\n", i, c.spk[:c.spl], offt)
		}

	}
	_ = cells
	_ = am

	cg := func(nibble int, skip bool) (*Cell, error) {
		return row[nibble], nil
	}

	be := NewBranchEncoder(1024, t.TempDir())
	enc, _, err := be.EncodeBranch(bm, bm, bm, cg)
	require.NoError(t, err)

	original := common.Copy(enc)

	target := make([]byte, 0, len(enc))
	oldKeys := make([][]byte, 0)
	replaced, err := enc.ReplacePlainKeys(target, func(key []byte, isStorage bool) ([]byte, error) {
		oldKeys = append(oldKeys, key)
		if isStorage {
			return key[:8], nil
		}
		return key[:4], nil
	})
	require.NoError(t, err)
	require.Truef(t, len(replaced) < len(enc), "replaced expected to be shorter than original enc")

	keyI := 0
	replacedBack, err := replaced.ReplacePlainKeys(nil, func(key []byte, isStorage bool) ([]byte, error) {
		require.EqualValues(t, oldKeys[keyI][:4], key[:4])
		defer func() { keyI++ }()
		return oldKeys[keyI], nil
	})
	require.NoError(t, err)
	require.EqualValues(t, original, replacedBack)

	t.Run("merge replaced and original back", func(t *testing.T) {
		orig := common.Copy(original)

		merged, err := replaced.MergeHexBranches(original, nil)
		require.NoError(t, err)
		require.EqualValues(t, orig, merged)

		merged, err = merged.MergeHexBranches(replacedBack, nil)
		require.NoError(t, err)
		require.EqualValues(t, orig, merged)
	})
}

func TestBranchData_ReplacePlainKeys_WithEmpty(t *testing.T) {
	row, bm := generateCellRow(t, 16)

	cg := func(nibble int, skip bool) (*Cell, error) {
		return row[nibble], nil
	}

	be := NewBranchEncoder(1024, t.TempDir())
	enc, _, err := be.EncodeBranch(bm, bm, bm, cg)
	require.NoError(t, err)

	original := common.Copy(enc)

	target := make([]byte, 0, len(enc))
	oldKeys := make([][]byte, 0)
	replaced, err := enc.ReplacePlainKeys(target, func(key []byte, isStorage bool) ([]byte, error) {
		oldKeys = append(oldKeys, key)
		if isStorage {
			return nil, nil
		}
		return nil, nil
	})
	require.NoError(t, err)
	require.EqualValuesf(t, len(enc), len(replaced), "replaced expected to be equal to origin (since no replacements were made)")

	keyI := 0
	replacedBack, err := replaced.ReplacePlainKeys(nil, func(key []byte, isStorage bool) ([]byte, error) {
		require.EqualValues(t, oldKeys[keyI][:4], key[:4])
		defer func() { keyI++ }()
		return oldKeys[keyI], nil
	})
	require.NoError(t, err)
	require.EqualValues(t, original, replacedBack)

	t.Run("merge replaced and original back", func(t *testing.T) {
		orig := common.Copy(original)

		merged, err := replaced.MergeHexBranches(original, nil)
		require.NoError(t, err)
		require.EqualValues(t, orig, merged)

		merged, err = merged.MergeHexBranches(replacedBack, nil)
		require.NoError(t, err)
		require.EqualValues(t, orig, merged)
	})
}
