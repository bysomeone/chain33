// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

// ParaTxIndexBuilder builds the para tx index of one main chain block.
//
// The index describes, for every para chain title in the block, where each
// para cross tx sits in the block, what its action type is and, for the
// actions that are actually executed, the full transaction. It is written to
// the local db next to the block and lets the executor of a later cross chain
// transaction rebuild that block's para transactions without reading the
// block body, which on a sharded node is no longer stored locally.
//
// The block body alone is not enough to build the index: which para txs count
// is decided by the dapp that owns them, so the builder is registered by that
// dapp (see RegParaTxIndexBuilder). chain33 itself only stores and serves what
// the builder returns.
type ParaTxIndexBuilder func(cfg *Chain33Config, detail *BlockDetail) ([]*HeightParaIndex, error)

var paraTxIndexBuilder ParaTxIndexBuilder

// RegParaTxIndexBuilder registers the para tx index builder.
// A block is saved without any index when no builder is registered.
func RegParaTxIndexBuilder(builder ParaTxIndexBuilder) {
	paraTxIndexBuilder = builder
}

// GetParaTxIndexBuilder returns the registered para tx index builder, nil when
// none is registered.
func GetParaTxIndexBuilder() ParaTxIndexBuilder {
	return paraTxIndexBuilder
}

// BuildParaTxIndex builds the para tx index of the block.
// It returns no index when no builder is registered.
func BuildParaTxIndex(cfg *Chain33Config, detail *BlockDetail) ([]*HeightParaIndex, error) {
	if paraTxIndexBuilder == nil {
		return nil, nil
	}
	return paraTxIndexBuilder(cfg, detail)
}
