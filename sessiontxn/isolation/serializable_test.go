// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package isolation_test

import (
	"context"
	"github.com/pingcap/tidb/executor"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessiontxn"
	"github.com/pingcap/tidb/sessiontxn/isolation"
	"github.com/pingcap/tidb/testkit"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPessimisticSerializableErrorTsCompare(t *testing.T) {
	store, _, clean := testkit.CreateMockStoreAndDomain(t)
	defer clean()

	tk := testkit.NewTestKit(t, store)
	se := tk.Session()
	provider := initializePessimisticSerializableProvider(t, tk)

	stmts, _, err := parser.New().Parse("select * from t", "", "")
	require.NoError(t, err)
	readOnlyStmt := stmts[0]

	stmts, _, err = parser.New().Parse("select * from t for update", "", "")
	require.NoError(t, err)
	forUpdateStmt := stmts[0]

	compareTS := getOracleTS(t, se)
	require.NoError(t, executor.ResetContextOfStmt(se, readOnlyStmt))
	require.NoError(t, provider.OnStmtStart(context.TODO()))
	ts, err := provider.GetStmtReadTS()
	require.NoError(t, err)
	require.Greater(t, compareTS, ts)
	prevTs := ts

	require.NoError(t, executor.ResetContextOfStmt(se, forUpdateStmt))
	require.NoError(t, provider.OnStmtStart(context.TODO()))
	ts, err = provider.GetStmtForUpdateTS()
	require.NoError(t, err)
	require.Greater(t, compareTS, ts)
	require.Equal(t, prevTs, ts)
}

func TestSerializableInitialize(t *testing.T) {
	store, _, clean := testkit.CreateMockStoreAndDomain(t)
	defer clean()

	tk := testkit.NewTestKit(t, store)
	se := tk.Session()
	tk.MustExec("set tidb_skip_isolation_level_check = 1")
	tk.MustExec("set @@tx_isolation = 'SERIALIZABLE'")
	tk.MustExec("set @@tidb_txn_mode='pessimistic'")

	// begin outsize a txn
	minStartTime := time.Now()
	tk.MustExec("begin")
	checkActiveSerializableTxn(t, se, minStartTime)

	// begin in a txn
	minStartTime = time.Now()
	tk.MustExec("begin")
	checkActiveRCTxn(t, se, minStartTime)

	// non-active txn and then active it
	tk.MustExec("rollback")
	tk.MustExec("set @@autocommit=0")
	minStartTime = time.Now()
	require.NoError(t, se.PrepareTxnCtx(context.TODO()))
	provider := checkNotActiveRCTxn(t, se, minStartTime)
	require.NoError(t, provider.OnStmtStart(context.TODO()))
	ts, err := provider.GetStmtReadTS()
	require.NoError(t, err)
	checkActiveRCTxn(t, se, minStartTime)
	require.Equal(t, ts, se.GetSessionVars().TxnCtx.StartTS)
	tk.MustExec("rollback")
}

func checkBasicSerializableTxn(t *testing.T, sctx sessionctx.Context,
	minStartTime time.Time) *isolation.PessimisticSerializableTxnContextProvider {
	provider := sessiontxn.GetTxnManager(sctx).GetContextProvider()
	require.IsType(t, &isolation.PessimisticSerializableTxnContextProvider{}, provider)

	sessVars := sctx.GetSessionVars()
	txnCtx := sessVars.TxnCtx

	if sessVars.SnapshotInfoschema == nil {
		require.Same(t, provider.GetTxnInfoSchema(), txnCtx.InfoSchema)
	} else {
		require.Equal(t, sessVars.SnapshotInfoschema.(infoschema.InfoSchema).SchemaMetaVersion(), provider.GetTxnInfoSchema().SchemaMetaVersion())
	}

	require.Equal(t, "SERIALIZABLE", txnCtx.Isolation)
	require.True(t, txnCtx.IsPessimistic)
	require.Equal(t, sctx.GetSessionVars().CheckAndGetTxnScope(), txnCtx.TxnScope)
	require.Equal(t, sessVars.ShardAllocateStep, int64(txnCtx.ShardStep))
	require.False(t, txnCtx.IsStaleness)
	require.GreaterOrEqual(t, txnCtx.CreateTime.Nanosecond(), minStartTime.Nanosecond())
	return provider.(*isolation.PessimisticSerializableTxnContextProvider)
}

func checkActiveSerializableTxn(t *testing.T, sctx sessionctx.Context,
	minStartTime time.Time) *isolation.PessimisticSerializableTxnContextProvider {
	sessVars := sctx.GetSessionVars()
	txnCtx := sessVars.TxnCtx
	provider := checkBasicSerializableTxn(t, sctx, minStartTime)

	txn, err := sctx.Txn(false)
	require.NoError(t, err)
	require.True(t, txn.Valid())
	require.Equal(t, txn.StartTS(), txnCtx.StartTS)
	require.True(t, txnCtx.IsExplicit)
	require.True(t, sessVars.InTxn())
	require.Same(t, sessVars.KVVars, txn.GetVars())
	return provider
}

func checkNotActiveSerializableTxn(t *testing.T,
	sctx sessionctx.Context, minStartTime time.Time) *isolation.PessimisticSerializableTxnContextProvider {
	sessVars := sctx.GetSessionVars()
	txnCtx := sessVars.TxnCtx
	provider := checkBasicSerializableTxn(t, sctx, minStartTime)

	txn, err := sctx.Txn(false)
	require.NoError(t, err)
	require.False(t, txn.Valid())
	require.Equal(t, uint64(0), txnCtx.StartTS)
	require.False(t, txnCtx.IsExplicit)
	require.False(t, sessVars.InTxn())
	return provider
}

func initializePessimisticSerializableProvider(t *testing.T,
	tk *testkit.TestKit) *isolation.PessimisticSerializableTxnContextProvider {
	tk.MustExec("set tidb_skip_isolation_level_check = 1")
	tk.MustExec("set @@tx_isolation = 'SERIALIZABLE'")
	minStartTime := time.Now()
	tk.MustExec("begin pessimistic")
	return checkActiveSerializableTxn(t, tk.Session(), minStartTime)
}
