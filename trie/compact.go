// Package trie provides slimtrie implementation.
//
// A slimtrie is a static, compressed Trie implementation.
// It is created from a standard Trie by removing unnecessary Trie-node.
// And it uses SlimArray to store Trie.
//
// slimtrie memory overhead is about 6 byte per key, or less.
//
// TODO benchmark
// TODO detail explain.
package trie

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/openacid/slim/array"
	"github.com/openacid/slim/bits"
	"github.com/openacid/slim/serialize"
	"github.com/openacid/slim/strhelper"
)

const (
	// WordMask is bit mask for extracting a 4-bit word.
	WordMask = 0xf
	// LeafWord is a special value to indicate a leaf node in a Trie.
	LeafWord = byte(0x10)
	// MaxNodeCnt is the max number of node(including leaf and inner node).
	MaxNodeCnt = 65536
)



// SlimTrie is a space efficient Trie index.
//
// The space overhead is about 6 byte per key and is irrelevant to key length.
//
// It does not store full key information, but only just enough info for
// locating a record.
// That's why an end user must re-validate the record key after reading it from
// other storage.
//
// It stores three parts of information in three SlimArray:
//
// `Children` stores node branches and children position.
// `Steps` stores the number of words to skip between a node and its parent.
// `Leaves` stores user data.
//
// TODO add scenario.
type SlimTrie struct {
	Children array.Array32
	Steps    StepArray
	Leaves   array.Array32
}

type StepArray struct {
	Array32Index
	Data []uint16
}

func (a *StepArray) Get2(idx int32) (uint16, bool) {
	i, ok := a.GetEltIndex(idx)
	if ok {
		return a.Data[i], true
	}

	return 0, false
}

type children struct {
	Bitmap uint16
	Offset uint16
}

var (
	// ErrTooManyTrieNodes indicates the number of trie nodes(not number of
	// keys) exceeded.
	ErrTooManyTrieNodes = errors.New("compacted trie exceeds max node count=65536")
	// ErrTrieBranchValueOverflow indicate input key consists of a word greater
	// than the max 4-bit word(0x0f).
	ErrTrieBranchValueOverflow = errors.New("compacted trie branch value must <=0x0f")
)

// childConv implements array.Converter and is the SlimArray adaptor for
// SlimTrie.Children .
type childConv struct {
	child *children
}

// Marshal
func (c childConv) Marshal(d interface{}) []byte {
	child := d.(*children)

	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[:2], child.Bitmap)
	binary.LittleEndian.PutUint16(b[2:4], child.Offset)

	return b
}

func (c childConv) Unmarshal(b []byte) (int, interface{}) {

	// Optimization: Use a containing struct to store and return it as return
	// value.
	// Note that this is not safe with concurrent uses of the `childConv` in
	// more than one go-routine.
	//
	// Avoid creating an object: mem-alloc is expensive
	//
	//	   var d interface{}
	//	   d = &children{
	//		   Bitmap: binary.LittleEndian.Uint16(b[:2]),
	//		   Offset: binary.LittleEndian.Uint16(b[2:4]),
	//	   }
	//	   return 4, d
	//
	// Avoid the following, it is even worse, converting struct `d` to
	// `interface{}` results in another mem-alloc:
	//
	//     d := children{
	//		   Bitmap: binary.LittleEndian.Uint16(b[:2]),
	//		   Offset: binary.LittleEndian.Uint16(b[2:4]),
	//     }
	//     return 4, d

	c.child.Bitmap = binary.LittleEndian.Uint16(b[:2])
	c.child.Offset = binary.LittleEndian.Uint16(b[2:4])

	return 4, c.child
}

func (c childConv) GetMarshaledSize(b []byte) int {
	return 4
}

// NewSlimTrie create an empty SlimTrie.
// Argument c implements a array.Converter to convert user data to serialized
// bytes and back.
func NewSlimTrie(c array.Converter, keys []string, values interface{}) (*SlimTrie, error) {
	var step uint16
	st := &SlimTrie{
		Children: array.Array32{Converter: childConv{child: &children{}}},
		Steps:    StepArray,
		Leaves:   array.Array32{Converter: c},
	}

	if keys != nil {
		return st, st.load(keys, values)
	}

	return st, nil
}

// load Loads keys and values and builds a SlimTrie.
//
// values must be a slice of data-type which is compatible with
// SlimTrie.Leaves.Converter .
func (st *SlimTrie) load(keys []string, values interface{}) (err error) {
	ks := strhelper.SliceToBitWords(keys, 4)
	return st.loadBytes(ks, values)
}

func (st *SlimTrie) loadBytes(keys [][]byte, values interface{}) (err error) {

	trie, err := NewTrie(keys, values)
	if err != nil {
		return err
	}

	trie.Squash()
	st.LoadTrie(trie)
	return nil
}

// LoadTrie compress a standard Trie and store compressed data in it.
func (st *SlimTrie) LoadTrie(root *Node) (err error) {
	if root == nil {
		return
	}

	childIndex, childData := []int32{}, []*children{}
	stepIndex := []int32{}
	leafIndex, leafData := []int32{}, []interface{}{}

	tq := make([]*Node, 0, 256)
	tq = append(tq, root)

	for nID := int32(0); ; {
		if len(tq) == 0 {
			break
		}

		node := tq[0]
		tq = tq[1:]

		if len(node.Branches) == 0 {
			continue
		}

		brs := node.Branches

		if brs[0] == leafBranch {
			leafIndex = append(leafIndex, nID)
			leafData = append(leafData, node.Children[brs[0]].Value)

			brs = brs[1:]
		}

		if node.Step > 1 {
			stepIndex = append(stepIndex, nID)
			st.Steps.Data = append(st.Steps.Data, node.Step)

		}

		if len(brs) > 0 {
			childIndex = append(childIndex, nID)
			offset := uint16(nID) + uint16(len(tq)) + uint16(1)

			bitmap := uint16(0)
			for _, b := range brs {
				if b&WordMask != b {
					return ErrTrieBranchValueOverflow
				}
				bitmap |= uint16(1) << (uint16(b) & WordMask)
			}

			ch := &children{
				Bitmap: bitmap,
				Offset: offset,
			}

			childData = append(childData, ch)
		}

		for _, b := range brs {
			tq = append(tq, node.Children[b])
		}

		nID++
		if nID > MaxNodeCnt {
			return ErrTooManyTrieNodes
		}
	}

	err = st.Children.Init(childIndex, childData)
	if err != nil {
		return err
	}

	err = st.Steps.InitIndexBitmap(stepIndex)
	if err != nil {
		return err
	}

	err = st.Leaves.Init(leafIndex, leafData)
	if err != nil {
		return err
	}

	return nil
}

// searchWords for a key in SlimTrie.
//
// `key` is slice of 4-bit word each stored in a byte.
// the higher 4 bit in byte is removed.
//
// It returns values of 3 keys:
// The value of greatest key < `key`. It is nil if `key` is the smallest.
// The value of `key`. It is nil if there is not a matching.
// The value of smallest key > `key`. It is nil if `key` is the greatest.
//
// A non-nil return value does not mean the `key` exists.
// An in-existent `key` also could matches partial info stored in SlimTrie.
func (st *SlimTrie) searchWords(key []byte) (ltVal, eqVal, gtVal interface{}) {
	eqIdx, ltIdx, gtIdx := int32(0), int32(-1), int32(-1)
	ltLeaf := false

	for idx := uint16(0); ; {
		var word byte
		if uint16(len(key)) == idx {
			word = LeafWord
		} else {
			word = (key[idx] & WordMask)
		}

		li, ei, ri, leaf := st.neighborBranches(uint16(eqIdx), word)
		if li >= 0 {
			ltIdx = li
			ltLeaf = leaf
		}

		if ri >= 0 {
			gtIdx = ri
		}

		eqIdx = ei
		if eqIdx == -1 {
			break
		}

		if word == LeafWord {
			break
		}

		idx += st.getStep(uint16(eqIdx))

		if idx > uint16(len(key)) {
			gtIdx = eqIdx
			eqIdx = -1
			break
		}
	}

	if ltIdx != -1 {
		if ltLeaf {
			ltVal = st.Leaves.Get(ltIdx)
		} else {
			rmIdx := st.rightMost(uint16(ltIdx))
			ltVal = st.Leaves.Get(int32(rmIdx))
		}
	}
	if gtIdx != -1 {
		fmIdx := st.leftMost(uint16(gtIdx))
		gtVal = st.Leaves.Get(int32(fmIdx))
	}
	if eqIdx != -1 {
		eqVal = st.Leaves.Get(eqIdx)
	}

	return
}

// Search for a key in SlimTrie.
//
// It returns values of 3 values:
// The value of greatest key < `key`. It is nil if `key` is the smallest.
// The value of `key`. It is nil if there is not a matching.
// The value of smallest key > `key`. It is nil if `key` is the greatest.
//
// A non-nil return value does not mean the `key` exists.
// An in-existent `key` also could matches partial info stored in SlimTrie.
func (st *SlimTrie) Search(key string) (ltVal, eqVal, gtVal interface{}) {
	eqIdx, ltIdx, gtIdx := int32(0), int32(-1), int32(-1)
	ltLeaf := false

	// string to 4-bit words
	lenWords := 2 * uint16(len(key))

	for idx := uint16(0); ; {
		var word byte
		if lenWords == idx {
			word = LeafWord
		} else {
			if idx&uint16(1) == uint16(1) {
				word = (key[idx>>1] & 0x0f)
			} else {
				word = (key[idx>>1] & 0xf0) >> 4
			}
		}

		li, ei, ri, leaf := st.neighborBranches(uint16(eqIdx), word)
		if li >= 0 {
			ltIdx = li
			ltLeaf = leaf
		}

		if ri >= 0 {
			gtIdx = ri
		}

		eqIdx = ei
		if eqIdx == -1 {
			break
		}

		if word == LeafWord {
			break
		}

		idx += st.getStep(uint16(eqIdx))

		if idx > lenWords {
			gtIdx = eqIdx
			eqIdx = -1
			break
		}
	}

	if ltIdx != -1 {
		if ltLeaf {
			ltVal = st.Leaves.Get(ltIdx)
		} else {
			rmIdx := st.rightMost(uint16(ltIdx))
			ltVal = st.Leaves.Get(int32(rmIdx))
		}
	}
	if gtIdx != -1 {
		fmIdx := st.leftMost(uint16(gtIdx))
		gtVal = st.Leaves.Get(int32(fmIdx))
	}
	if eqIdx != -1 {
		eqVal = st.Leaves.Get(eqIdx)
	}

	return
}

// just return equal value for trie.Search benchmark

// Get the value of the specified key from SlimTrie.
//
// If the key exist in SlimTrie, it returns the correct value.
// If the key does NOT exist in SlimTrie, it could also return some value.
//
// Because SlimTrie is a "index" but not a "kv-map", it does not stores complete
// info of all keys.
// SlimTrie tell you "WHERE IT POSSIBLY BE", rather than "IT IS JUST THERE".
func (st *SlimTrie) Get(key string) (eqVal interface{}) {

	var word byte
	eqIdx := int32(0)

	// string to 4-bit words
	lenWords := 2 * uint16(len(key))

	for idx := uint16(0); ; {
		if lenWords == idx {
			break
		}

		// Get a 4-bit word from 8-bit words.
		// Use arithmetic to avoid branch missing.
		shift := 4 - (idx&1)*4
		word = ((key[idx>>1] >> shift) & 0x0f)

		eqIdx = st.nextBranch(uint16(eqIdx), word)
		if eqIdx == -1 {
			break
		}

		idx += st.getStep(uint16(eqIdx))

		if idx > lenWords {
			eqIdx = -1
			break
		}
	}

	if eqIdx != -1 {
		eqVal = st.Leaves.Get(eqIdx)
	}

	return
}

func (st *SlimTrie) getChild(idx uint16) *children {
	cval, found := st.Children.Get2(int32(idx))
	if found {
		return cval.(*children)
	}
	return nil
}

func (st *SlimTrie) getStep(idx uint16) uint16 {
	step, found := st.Steps.Get2(int32(idx))
	if found {
		return step
	}
	return uint16(1)
}

// getChildIdx returns the id of the specified child.
// This function does not check if the specified child `offset` exists or not.
func getChildIdx(ch *children, word uint16) uint16 {
	chNum := bits.OnesCount64Before(uint64(ch.Bitmap), uint(word))
	return ch.Offset + uint16(chNum)
}

func (st *SlimTrie) neighborBranches(idx uint16, word byte) (ltIdx, eqIdx, rtIdx int32, ltLeaf bool) {
	ltIdx, eqIdx, rtIdx = int32(-1), int32(-1), int32(-1)
	ltLeaf = false

	isLeaf := st.Leaves.Has(int32(idx))

	if word == LeafWord {
		if isLeaf {
			eqIdx = int32(idx)
		}
	} else {
		if isLeaf {
			ltIdx = int32(idx)
			ltLeaf = true
		}
	}

	ch := st.getChild(idx)
	if ch == nil {
		return
	}

	if (ch.Bitmap >> word & 1) == 1 {
		eqIdx = int32(getChildIdx(ch, uint16(word)))
	}

	ltStart := word & WordMask
	for i := int8(ltStart) - 1; i >= 0; i-- {
		if (ch.Bitmap >> uint8(i) & 1) == 1 {
			ltIdx = int32(getChildIdx(ch, uint16(i)))
			ltLeaf = false
			break
		}
	}

	rtStart := word + 1
	if word == LeafWord {
		rtStart = uint8(0)
	}

	for i := rtStart; i < LeafWord; i++ {
		if (ch.Bitmap >> i & 1) == 1 {
			rtIdx = int32(getChildIdx(ch, uint16(i)))
			break
		}
	}

	return
}

func (st *SlimTrie) nextBranch(idx uint16, word byte) int32 {

	ch := st.getChild(idx)
	if ch == nil {
		return -1
	}

	if (ch.Bitmap >> word & 1) == 1 {
		return int32(getChildIdx(ch, uint16(word)))
	}

	return -1
}

func (st *SlimTrie) leftMost(idx uint16) uint16 {
	for {
		if st.Leaves.Has(int32(idx)) {
			return idx
		}

		ch := st.getChild(idx)
		idx = ch.Offset
	}
}

func (st *SlimTrie) rightMost(idx uint16) uint16 {
	for {
		// TODO performance: just call getChild directly
		if !st.Children.Has(int32(idx)) {
			return idx
		}

		ch := st.getChild(idx)

		// count number of all children
		// TODO use bits.PopCntXX without before.
		chNum := bits.OnesCount64Before(uint64(ch.Bitmap), 64)
		idx = ch.Offset + uint16(chNum-1)

	}
}

// getMarshalSize returns the serialized length in byte of a SlimTrie.
func (st *SlimTrie) getMarshalSize() int64 {
	cSize := serialize.GetMarshalSize(&st.Children)
	sSize := serialize.GetMarshalSize(&st.Steps)
	lSize := serialize.GetMarshalSize(&st.Leaves)

	return cSize + sSize + lSize
}

// marshal serializes it to byte stream.
func (st *SlimTrie) marshal(writer io.Writer) (cnt int64, err error) {
	var n int64

	if n, err = serialize.Marshal(writer, &st.Children); err != nil {
		return 0, err
	}
	cnt += n

	if n, err = serialize.Marshal(writer, &st.Steps); err != nil {
		return 0, err
	}
	cnt += n

	if n, err = serialize.Marshal(writer, &st.Leaves); err != nil {
		return 0, err
	}
	cnt += n

	return cnt, nil
}

// marshalAt serializes it to byte stream and write the stream at specified
// offset.
// TODO change to io.WriterAt
func (st *SlimTrie) marshalAt(f *os.File, offset int64) (cnt int64, err error) {

	buf := new(bytes.Buffer)
	if cnt, err = st.marshal(buf); err != nil {
		return 0, err
	}

	if _, err = f.WriteAt(buf.Bytes(), offset); err != nil {
		return 0, err
	}

	return cnt, nil
}

// unmarshal de-serializes and loads SlimTrie from a byte stream.
func (st *SlimTrie) unmarshal(reader io.Reader) error {
	if err := serialize.Unmarshal(reader, &st.Children); err != nil {
		return err
	}

	if err := serialize.Unmarshal(reader, &st.Steps); err != nil {
		return err
	}

	if err := serialize.Unmarshal(reader, &st.Leaves); err != nil {
		return err
	}

	return nil
}

// Unmarshal de-serializes and loads SlimTrie from a byte stream at
// specified offset.
// TODO change to io.ReaderAt
func (st *SlimTrie) unmarshalAt(f *os.File, offset int64) (n int64, err error) {
	childrenSize, err := serialize.UnmarshalAt(f, offset, &st.Children)
	if err != nil {
		return n, err
	}
	offset += childrenSize

	stepsSize, err := serialize.UnmarshalAt(f, offset, &st.Steps)
	if err != nil {
		return n, err
	}
	offset += stepsSize

	leavesSize, err := serialize.UnmarshalAt(f, offset, &st.Leaves)
	if err != nil {
		return n, err
	}

	n = childrenSize + stepsSize + leavesSize
	return n, nil
}
