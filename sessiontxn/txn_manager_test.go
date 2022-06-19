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

package sessiontxn_test

import (
	"context"
	"fmt"
	"github.com/pingcap/failpoint"
	"testing"
	"time"

	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessiontxn"
	"github.com/pingcap/tidb/sessiontxn/staleread"
	"github.com/pingcap/tidb/testkit"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/oracle"
)

func TestEnterNewTxn(t *testing.T) {
	store, _, clean := testkit.CreateMockStoreAndDomain(t)
	defer clean()

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create temporary table tmp(a int)")
	tk.MustExec("create table t1(id int primary key)")
	is1 := domain.GetDomain(tk.Session()).InfoSchema()
	tk.MustExec("begin")
	stalenessTS, err := tk.Session().GetStore().GetOracle().GetTimestamp(context.Background(), &oracle.Option{TxnScope: oracle.GlobalTxnScope})
	require.NoError(t, err)
	tk.MustExec("create table t2(id int primary key)")

	mgr := sessiontxn.GetTxnManager(tk.Session())

	cases := []struct {
		name    string
		prepare func(t *testing.T)
		request *sessiontxn.EnterNewTxnRequest
		check   func(t *testing.T, sctx sessionctx.Context)
	}{
		{
			name: "EnterNewTxnDefault",
			request: &sessiontxn.EnterNewTxnRequest{
				Type: sessiontxn.EnterNewTxnDefault,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				txn := checkBasicActiveTxn(t, sctx)
				checkInfoSchemaVersion(t, sctx, domain.GetDomain(sctx).InfoSchema().SchemaMetaVersion())

				sessVars := sctx.GetSessionVars()
				require.False(t, txn.IsPessimistic())
				require.False(t, sessVars.InTxn())
				require.False(t, sessVars.TxnCtx.IsStaleness)

				require.False(t, sctx.GetSessionVars().TxnCtx.CouldRetry)
				require.True(t, txn.GetOption(kv.GuaranteeLinearizability).(bool))
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt simple",
			request: &sessiontxn.EnterNewTxnRequest{
				Type: sessiontxn.EnterNewTxnWithBeginStmt,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnWithBeginStmt(t, sctx, false, true)
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt with tidb_txn_mode=pessimistic",
			prepare: func(t *testing.T) {
				tk.MustExec("set @@tidb_txn_mode='pessimistic'")
			},
			request: &sessiontxn.EnterNewTxnRequest{
				Type:    sessiontxn.EnterNewTxnWithBeginStmt,
				TxnMode: ast.Pessimistic,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnWithBeginStmt(t, sctx, true, true)
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt with tidb_txn_mode=pessimistic;begin optimistic",
			prepare: func(t *testing.T) {
				tk.MustExec("set @@tidb_txn_mode='pessimistic'")
			},
			request: &sessiontxn.EnterNewTxnRequest{
				Type:    sessiontxn.EnterNewTxnWithBeginStmt,
				TxnMode: ast.Optimistic,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnWithBeginStmt(t, sctx, false, true)
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt with tidb_txn_mode=optimistic;begin pessimistic",
			prepare: func(t *testing.T) {
				tk.MustExec("set @@tidb_txn_mode='optimistic'")
			},
			request: &sessiontxn.EnterNewTxnRequest{
				Type:    sessiontxn.EnterNewTxnWithBeginStmt,
				TxnMode: ast.Pessimistic,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnWithBeginStmt(t, sctx, true, true)
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt with CausalConsistencyOnly",
			request: &sessiontxn.EnterNewTxnRequest{
				Type:                  sessiontxn.EnterNewTxnWithBeginStmt,
				CausalConsistencyOnly: true,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnWithBeginStmt(t, sctx, false, false)
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt and reuse txn",
			prepare: func(t *testing.T) {
				err := mgr.EnterNewTxn(context.TODO(), &sessiontxn.EnterNewTxnRequest{
					Type: sessiontxn.EnterNewTxnBeforeStmt,
				})
				require.NoError(t, err)
				require.NoError(t, mgr.OnStmtStart(context.TODO()))
				require.NoError(t, mgr.AdviseWarmup())
			},
			request: &sessiontxn.EnterNewTxnRequest{
				Type: sessiontxn.EnterNewTxnWithBeginStmt,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnWithBeginStmt(t, sctx, false, true)
			},
		},
		{
			name: "EnterNewTxnWithBeginStmt with staleness",
			request: &sessiontxn.EnterNewTxnRequest{
				Type:        sessiontxn.EnterNewTxnWithBeginStmt,
				StaleReadTS: stalenessTS,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				txn := checkBasicActiveTxn(t, sctx)
				require.Equal(t, stalenessTS, txn.StartTS())
				is := sessiontxn.GetTxnManager(sctx).GetTxnInfoSchema()
				require.Equal(t, is1.SchemaMetaVersion(), is.SchemaMetaVersion())
				_, ok := is.(*infoschema.TemporaryTableAttachedInfoSchema)
				require.True(t, ok)

				sessVars := sctx.GetSessionVars()
				require.Equal(t, false, txn.IsPessimistic())
				require.True(t, sessVars.InTxn())
				require.True(t, sessVars.TxnCtx.IsStaleness)
			},
		},
		{
			name: "EnterNewTxnBeforeStmt",
			request: &sessiontxn.EnterNewTxnRequest{
				Type: sessiontxn.EnterNewTxnBeforeStmt,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnBeforeStmt(t, sctx)
				checkStmtTxnAfterActive(t, sctx)
				require.True(t, sctx.GetSessionVars().TxnCtx.CouldRetry)
				require.False(t, sctx.GetSessionVars().InTxn())
			},
		},
		{
			name: "EnterNewTxnBeforeStmt and autocommit=0",
			prepare: func(t *testing.T) {
				tk.MustExec("set @@autocommit=0")
			},
			request: &sessiontxn.EnterNewTxnRequest{
				Type: sessiontxn.EnterNewTxnBeforeStmt,
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkTxnBeforeStmt(t, sctx)
				checkStmtTxnAfterActive(t, sctx)
				require.False(t, sctx.GetSessionVars().TxnCtx.CouldRetry)
				require.True(t, sctx.GetSessionVars().InTxn())
			},
		},
		{
			name: "EnterNewTxnWithReplaceProvider",
			prepare: func(t *testing.T) {
				err := mgr.EnterNewTxn(context.TODO(), &sessiontxn.EnterNewTxnRequest{
					Type: sessiontxn.EnterNewTxnBeforeStmt,
				})
				require.NoError(t, err)
			},
			request: &sessiontxn.EnterNewTxnRequest{
				Type:     sessiontxn.EnterNewTxnWithReplaceProvider,
				Provider: staleread.NewStalenessTxnContextProvider(tk.Session(), stalenessTS, nil),
			},
			check: func(t *testing.T, sctx sessionctx.Context) {
				checkNonActiveStalenessTxn(t, sctx, stalenessTS, is1)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			se := tk.Session()
			tk.MustExec("set @@tidb_txn_mode=''")
			tk.MustExec("set @@autocommit=1")
			tk.MustExec("commit")

			if c.prepare != nil {
				c.prepare(t)
			}

			require.NoError(t, mgr.EnterNewTxn(context.TODO(), c.request))
			if c.check != nil {
				c.check(t, se)
			}
		})
	}
}

func checkBasicActiveTxn(t *testing.T, sctx sessionctx.Context) kv.Transaction {
	txn, err := sctx.Txn(false)
	require.NoError(t, err)
	require.True(t, txn.Valid())

	sessVars := sctx.GetSessionVars()

	require.Equal(t, txn.IsPessimistic(), sessVars.TxnCtx.IsPessimistic)
	require.Equal(t, txn.StartTS(), sessVars.TxnCtx.StartTS)
	require.Equal(t, sessVars.InTxn(), sessVars.TxnCtx.IsExplicit)
	return txn
}

func checkTxnWithBeginStmt(t *testing.T, sctx sessionctx.Context, pessimistic, guaranteeLinear bool) {
	txn := checkBasicActiveTxn(t, sctx)
	checkInfoSchemaVersion(t, sctx, domain.GetDomain(sctx).InfoSchema().SchemaMetaVersion())

	sessVars := sctx.GetSessionVars()
	require.Equal(t, pessimistic, txn.IsPessimistic())
	require.True(t, sessVars.InTxn())
	require.False(t, sessVars.TxnCtx.IsStaleness)

	require.False(t, sctx.GetSessionVars().TxnCtx.CouldRetry)
	require.Equal(t, guaranteeLinear, txn.GetOption(kv.GuaranteeLinearizability).(bool))
}

func checkTxnBeforeStmt(t *testing.T, sctx sessionctx.Context) {
	txn, err := sctx.Txn(false)
	require.NoError(t, err)
	require.False(t, txn.Valid())
	checkInfoSchemaVersion(t, sctx, domain.GetDomain(sctx).InfoSchema().SchemaMetaVersion())

	sessVars := sctx.GetSessionVars()
	require.False(t, sessVars.TxnCtx.IsPessimistic)
	require.False(t, sessVars.TxnCtx.IsExplicit)
	require.False(t, sessVars.InTxn())
	require.False(t, sessVars.TxnCtx.CouldRetry)
	require.False(t, sessVars.TxnCtx.IsStaleness)
}

func checkStmtTxnAfterActive(t *testing.T, sctx sessionctx.Context) {
	require.NoError(t, sessiontxn.GetTxnManager(sctx).AdviseWarmup())
	_, err := sctx.Txn(true)
	require.NoError(t, err)
	txn := checkBasicActiveTxn(t, sctx)
	checkInfoSchemaVersion(t, sctx, domain.GetDomain(sctx).InfoSchema().SchemaMetaVersion())

	sessVars := sctx.GetSessionVars()
	require.False(t, txn.IsPessimistic())
	require.False(t, sessVars.TxnCtx.IsStaleness)
}

func checkNonActiveStalenessTxn(t *testing.T, sctx sessionctx.Context, ts uint64, is infoschema.InfoSchema) {
	txn, err := sctx.Txn(false)
	require.NoError(t, err)
	require.False(t, txn.Valid())
	require.False(t, sctx.GetSessionVars().InTxn())
	require.False(t, sctx.GetSessionVars().TxnCtx.IsExplicit)
	require.True(t, sctx.GetSessionVars().TxnCtx.IsStaleness)
	checkInfoSchemaVersion(t, sctx, is.SchemaMetaVersion())
	readTS, err := sessiontxn.GetTxnManager(sctx).GetStmtReadTS()
	require.NoError(t, err)
	require.Equal(t, ts, readTS)
}

func checkInfoSchemaVersion(t *testing.T, sctx sessionctx.Context, ver int64) {
	is1, ok := sessiontxn.GetTxnManager(sctx).GetTxnInfoSchema().(*infoschema.TemporaryTableAttachedInfoSchema)
	require.True(t, ok)

	is2 := sctx.GetSessionVars().TxnCtx.InfoSchema.(*infoschema.TemporaryTableAttachedInfoSchema)
	require.True(t, ok)

	require.Equal(t, ver, is1.SchemaMetaVersion())
	require.Equal(t, ver, is2.SchemaMetaVersion())
}

func TestConflictErrorInOtherQueryContainingPointGet111(t *testing.T) {
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/executor/hookBeforeRunChildrenNextInSelectLock", "return"))
	store, _, clean := testkit.CreateMockStoreAndDomain(t)
	defer clean()

	tk := testkit.NewTestKit(t, store)
	se := tk.Session()
	//tk2 := testkit.NewTestKit(t, store)

	wait := func(ch chan string, expect string) {
		select {
		case got := <-ch:
			if got != expect {
				panic(fmt.Sprintf("expect '%s', got '%s'", expect, got))
			}
		case <-time.After(time.Second * 10):
			panic("wait2 timeout")
		}
	}

	chanBeforeRunNextInSelectLock := make(chan func(), 1)
	c2 := make(chan string, 1)
	c3 := make(chan string, 1)

	se.SetValue(sessiontxn.HookBeforeRunChildrenNextInSelectLock, chanBeforeRunNextInSelectLock)

	chanBeforeRunNextInSelectLock <- func() {
		c2 <- "before run children's next in select lock"
		wait(c3, "insert done")
	}

	tk.MustExec("use test")
	//tk2.MustExec("use test")
	tk.MustExec("create table t (id int primary key, v int)")

	go func() {
		tk2 := testkit.NewTestKit(t, store)
		tk2.MustExec("use test")

		wait(c2, "before run children's next in select lock")
		tk2.MustExec("insert into t values (1, 1), (2,2),(3,3),(4,4)")
		c3 <- "insert done"

	}()
	//tk2.MustExec("insert into t values (1, 1), (2,2),(3,3),(4,4)")

	tk.MustExec("begin pessimistic")
	//tk2.MustExec("insert into t values (1, 1), (2,2)")
	//tk2.MustExec("update t set v = v + 5 where id = 1")
	//tk.MustQuery("select * from t where id=1 for update union all select * from t where id = 2 for update").Check(testkit.Rows("1 1", "2 2"))
	tk.MustQuery("select * from t for update").Check(testkit.Rows("1 1", "2 2", "1 1", "2 2"))
	//tk.MustExec("select * from t where id = 1 and v > 1 for update")
	//tk.MustExec("select * from t where id in (1, 2, 3) order by id for update")
	//records, ok := se.Value(sessiontxn.AssertLockErr).(map[string]int)
	//require.True(t, ok)
	//require.Equal(t, records["errWriteConflict"], 1)

	tk.MustExec("rollback")
}

func TestConflictErrorInOtherQueryContainingPointGet11o1(t *testing.T) {
	store, _, clean := testkit.CreateMockStoreAndDomain(t)
	defer clean()

	tk := testkit.NewTestKit(t, store)

	tk2 := testkit.NewTestKit(t, store)

	tk.MustExec("use test")
	tk2.MustExec("use test")
	tk.MustExec("create table t (id int primary key, v int)")

	//tk2.MustExec("insert into t values (1, 1), (2,2),(3,3),(4,4)")

	tk.MustExec("begin pessimistic")
	tk2.MustExec("insert into t values (1, 1), (2,2)")
	//tk2.MustExec("update t set v = v + 5 where id = 1")
	//tk.MustQuery("select * from t where id = 1 for update").Check(testkit.Rows("1 1"))
	tk.MustQuery("select * from t where id=1 for update union all select * from t where id = 2 for update").Check(testkit.Rows("1 1", "2 2"))
	//tk.MustExec("select * from t where id = 1 and v > 1 for update")
	//tk.MustExec("select * from t where id in (1, 2, 3) order by id for update")
	//records, ok := se.Value(sessiontxn.AssertLockErr).(map[string]int)
	//require.True(t, ok)
	//require.Equal(t, records["errWriteConflict"], 1)

	tk.MustExec("rollback")
}
