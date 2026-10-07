// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	dbm "github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/stretchr/testify/require"
)

const (
	paraTxIndexTestTitle  = "user.p.test."
	paraTxIndexTestHeight = int64(100)
	paraTxIndexTestPrefix = "CHAIN-paratx-index"
)

// regParaTxIndexBuilder 注册索引构建器, 用例结束后恢复原值
func regParaTxIndexBuilder(t *testing.T, builder types.ParaTxIndexBuilder) {
	old := types.GetParaTxIndexBuilder()
	types.RegParaTxIndexBuilder(builder)
	t.Cleanup(func() {
		types.RegParaTxIndexBuilder(old)
	})
}

func newParaTxIndexTestDB(t *testing.T) dbm.DB {
	dir, err := ioutil.TempDir("", "paratxindex")
	require.Nil(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dbm.NewDB("paratxindex", "memdb", dir, 1)
}

// stubParaTxIndexBuilder 返回固定索引的构建器, chain33侧用例不需要plugin参与
func stubParaTxIndexBuilder(cfg *types.Chain33Config, detail *types.BlockDetail) ([]*types.HeightParaIndex, error) {
	return []*types.HeightParaIndex{{
		Height: detail.Block.Height,
		Title:  paraTxIndexTestTitle,
		Hash:   detail.Block.Hash(cfg),
		Items: []*types.ParaTxIndexItem{
			{BaseIndex: 0, Ty: 0},
			{BaseIndex: 2, Ty: 10000, Tx: []byte("asset tx")},
		},
	}}, nil
}

// TestParaTxIndexWithoutBuilder 未注册builder时不能产生任何索引数据
func TestParaTxIndexWithoutBuilder(t *testing.T) {
	regParaTxIndexBuilder(t, nil)
	require.Nil(t, types.GetParaTxIndexBuilder())

	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	db := newParaTxIndexTestDB(t)
	detail := &types.BlockDetail{Block: &types.Block{Height: paraTxIndexTestHeight}}

	indexes, err := buildParaTxIndex(cfg, detail)
	require.NoError(t, err)
	require.Len(t, indexes, 0)

	kvs, err := saveParaTxIndexForBlock(cfg, db, detail)
	require.NoError(t, err)
	require.Len(t, kvs, 0)

	// 读路径必须拿不到索引, 调用方据此回退到读取区块体
	_, err = getParaTxIndex(db, paraTxIndexTestHeight, paraTxIndexTestTitle)
	require.Error(t, err)
}

// TestParaTxIndexSaveReadDelete 索引的写入/读取/删除
func TestParaTxIndexSaveReadDelete(t *testing.T) {
	regParaTxIndexBuilder(t, stubParaTxIndexBuilder)
	require.NotNil(t, types.GetParaTxIndexBuilder())

	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	db := newParaTxIndexTestDB(t)
	detail := &types.BlockDetail{Block: &types.Block{Height: paraTxIndexTestHeight}}

	kvs, err := saveParaTxIndexForBlock(cfg, db, detail)
	require.NoError(t, err)
	require.NotEmpty(t, kvs)
	for _, kv := range kvs {
		require.NoError(t, db.Set(kv.GetKey(), kv.GetValue()))
	}

	index, err := getParaTxIndex(db, paraTxIndexTestHeight, paraTxIndexTestTitle)
	require.NoError(t, err)
	require.Equal(t, paraTxIndexTestHeight, index.GetHeight())
	require.Equal(t, paraTxIndexTestTitle, index.GetTitle())
	require.Equal(t, detail.Block.Hash(cfg), index.GetHash())
	require.Len(t, index.GetItems(), 2)

	// 其它高度/title读不到
	_, err = getParaTxIndex(db, paraTxIndexTestHeight+1, paraTxIndexTestTitle)
	require.Error(t, err)
	_, err = getParaTxIndex(db, paraTxIndexTestHeight, "user.p.other.")
	require.Error(t, err)

	delKvs, err := delParaTxIndexTable(db, paraTxIndexTestHeight)
	require.NoError(t, err)
	require.NotEmpty(t, delKvs)
	for _, kv := range delKvs {
		if len(kv.GetKey()) != 0 && kv.GetValue() == nil {
			require.NoError(t, db.Delete(kv.GetKey()))
		}
	}
	_, err = getParaTxIndex(db, paraTxIndexTestHeight, paraTxIndexTestTitle)
	require.Error(t, err)
}

// TestParaTxIndexBuilderPanic 构建器panic不能影响区块保存
func TestParaTxIndexBuilderPanic(t *testing.T) {
	regParaTxIndexBuilder(t, func(cfg *types.Chain33Config, detail *types.BlockDetail) ([]*types.HeightParaIndex, error) {
		panic("builder boom")
	})

	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	db := newParaTxIndexTestDB(t)
	detail := &types.BlockDetail{Block: &types.Block{Height: paraTxIndexTestHeight}}

	indexes, err := buildParaTxIndex(cfg, detail)
	require.Error(t, err)
	require.Nil(t, indexes)

	kvs, err := saveParaTxIndexForBlock(cfg, db, detail)
	require.Error(t, err)
	require.Len(t, kvs, 0)
}

// dumpDBKeys 读出db中所有key
func dumpDBKeys(t *testing.T, db dbm.DB) [][]byte {
	it := db.Iterator(nil, nil, false)
	defer it.Close()
	var keys [][]byte
	for it.Next() {
		keys = append(keys, it.Key())
	}
	require.NoError(t, it.Error())
	return keys
}

// TestSaveBlockForTableIndexDelta 注册builder后新增的KV只能是索引表的记录,
// 未注册时saveBlockForTable写出的KV与不带索引逻辑时完全一致
func TestSaveBlockForTableIndexDelta(t *testing.T) {
	txs := []*types.Transaction{
		{Execer: []byte("user.p.test.paracross"), Payload: []byte("tx1")},
		{Execer: []byte("user.p.test.coins"), Payload: []byte("tx2")},
	}
	receipts := []*types.ReceiptData{{Ty: types.ExecOk}, {Ty: types.ExecOk}}

	write := func(register bool) map[string][]byte {
		if register {
			regParaTxIndexBuilder(t, stubParaTxIndexBuilder)
		} else {
			regParaTxIndexBuilder(t, nil)
		}
		chain := InitEnv()
		db := newParaTxIndexTestDB(t)
		blockStore := NewBlockStore(chain, db, chain.client)
		require.NotNil(t, blockStore)

		detail := &types.BlockDetail{
			Block:    &types.Block{Height: paraTxIndexTestHeight, Txs: txs},
			Receipts: receipts,
		}
		batch := blockStore.NewBatch(true)
		require.NoError(t, blockStore.saveBlockForTable(batch, detail, true, true))
		require.NoError(t, batch.Write())

		values := make(map[string][]byte)
		for _, key := range dumpDBKeys(t, db) {
			value, err := db.Get(key)
			require.NoError(t, err)
			values[string(key)] = value
		}
		return values
	}

	without := write(false)
	with := write(true)

	// 未注册builder时不能有任何索引记录
	for key := range without {
		require.False(t, bytes.HasPrefix([]byte(key), []byte(paraTxIndexTestPrefix)),
			"unexpected index key %s", key)
	}
	// 注册builder后, 新增的key必须全部是索引记录
	added := 0
	for key, value := range with {
		if _, exist := without[key]; exist {
			require.True(t, bytes.Equal(without[key], value), "value changed for key %s", key)
			continue
		}
		require.True(t, bytes.HasPrefix([]byte(key), []byte(paraTxIndexTestPrefix)),
			"unexpected extra key %s", key)
		added++
	}
	require.True(t, added > 0, "no index key added")
	// 反之, 未注册时也不能比注册时多出key
	for key := range without {
		_, exist := with[key]
		require.True(t, exist, "key %s missing when builder registered", key)
	}
	fmt.Println("keys without builder:", len(without), "with builder:", len(with))
}
