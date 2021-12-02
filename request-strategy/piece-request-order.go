package request_strategy

import (
	"github.com/anacrolix/torrent/metainfo"
	"github.com/google/btree"
)

func NewPieceOrder() *PieceRequestOrder {
	return &PieceRequestOrder{
		tree: btree.New(32),
		keys: make(map[PieceRequestOrderKey]PieceRequestOrderState),
	}
}

type PieceRequestOrder struct {
	tree *btree.BTree
	keys map[PieceRequestOrderKey]PieceRequestOrderState
}

type PieceRequestOrderKey struct {
	InfoHash metainfo.Hash
	Index    int
}

type PieceRequestOrderState struct {
	Priority     piecePriority
	Partial      bool
	Availability int64
}

type pieceRequestOrderItem struct {
	key   PieceRequestOrderKey
	state PieceRequestOrderState
}

func (me pieceRequestOrderItem) Less(other btree.Item) bool {
	otherConcrete := other.(pieceRequestOrderItem)
	return pieceOrderLess(
		pieceOrderInput{
			PieceRequestOrderState: me.state,
			PieceRequestOrderKey:   me.key,
		},
		pieceOrderInput{
			PieceRequestOrderState: otherConcrete.state,
			PieceRequestOrderKey:   otherConcrete.key,
		},
	).Less()
}

func (me *PieceRequestOrder) Add(key PieceRequestOrderKey, state PieceRequestOrderState) {
	if _, ok := me.keys[key]; ok {
		panic(key)
	}
	if me.tree.ReplaceOrInsert(pieceRequestOrderItem{
		key:   key,
		state: state,
	}) != nil {
		panic("shouldn't already have this")
	}
	me.keys[key] = state
}

func (me *PieceRequestOrder) Update(key PieceRequestOrderKey, state PieceRequestOrderState) {
	if me.tree.Delete(me.existingItemForKey(key)) == nil {
		panic(key)
	}
	if me.tree.ReplaceOrInsert(pieceRequestOrderItem{
		key:   key,
		state: state,
	}) != nil {
		panic(key)
	}
	me.keys[key] = state
}

func (me *PieceRequestOrder) existingItemForKey(key PieceRequestOrderKey) pieceRequestOrderItem {
	return pieceRequestOrderItem{
		key:   key,
		state: me.keys[key],
	}
}

func (me *PieceRequestOrder) Delete(key PieceRequestOrderKey) {
	if me.tree.Delete(me.existingItemForKey(key)) == nil {
		panic(key)
	}
	delete(me.keys, key)
}
