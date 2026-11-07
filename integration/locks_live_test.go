//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func lockRequest(session, lockID, requestID string) *protocol.LockRequest {
	return &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: session, LockId: lockID, Name: "ptah_schema_apply"}, RequestId: requestID}
}

// tryLock returns the outcome of one attempt. The SDK client reports an
// outcome other than ACQUIRED as an error beside the receipt, so the receipt's
// outcome is what the tests compare; a failed call has no receipt and reads as
// UNKNOWN, which no assertion expects.
func tryLock(c *qt.C, client *providerruntime.Client, session, lockID, requestID string) protocol.LockResult_Outcome {
	c.Helper()
	result, _ := client.TryLock(c.Context(), lockRequest(session, lockID, requestID))
	return result.GetOutcome()
}

// Two sessions exclude each other through the lock table, a released lock is
// taken by the next attempt, and a closed session gives its lock back.
func TestLocksSubprocess(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	first := openSession(c, client, address)
	second := openSession(c, client, address)

	c.Assert(tryLock(c, client, first.GetId(), "a1", "r1"), qt.Equals, protocol.LockResult_ACQUIRED)
	c.Assert(tryLock(c, client, second.GetId(), "b1", "r2"), qt.Equals, protocol.LockResult_BUSY)
	checked, err := client.CheckLock(c.Context(), lockRequest(first.GetId(), "a1", "r3"))
	c.Assert(err, qt.IsNil)
	c.Assert(checked.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)
	released, err := client.ReleaseLock(c.Context(), lockRequest(first.GetId(), "a1", "r4"))
	c.Assert(err, qt.IsNil)
	c.Assert(released.GetOutcome(), qt.Equals, protocol.LockResult_RELEASED)
	// A busy lock ID is used up; a new attempt takes a new one.
	c.Assert(tryLock(c, client, second.GetId(), "b1", "r5"), qt.Equals, protocol.LockResult_BUSY)
	c.Assert(tryLock(c, client, second.GetId(), "b2", "r6"), qt.Equals, protocol.LockResult_ACQUIRED)
	c.Assert(client.CloseSession(c.Context(), second.GetId()), qt.IsNil)
	c.Assert(tryLock(c, client, first.GetId(), "a2", "r7"), qt.Equals, protocol.LockResult_ACQUIRED)
}

// A holder whose lease has ended, as a process that died stops checking its
// lock, loses the lock to the next attempt and is fenced: its next check
// reports the loss and it runs no statement.
func TestLocksSubprocess_LeaseEnds(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	holder := openSession(c, client, address)
	other := openSession(c, client, address)
	c.Assert(tryLock(c, client, holder.GetId(), "h1", "r1"), qt.Equals, protocol.LockResult_ACQUIRED)
	execSQL(c, address, `UPDATE ptah_locks SET expires_at = CURRENT_TIMESTAMP - INTERVAL '1 seconds' WHERE lock_name = 'ptah_schema_apply'`)

	c.Assert(tryLock(c, client, other.GetId(), "o1", "r2"), qt.Equals, protocol.LockResult_ACQUIRED)
	checked, err := client.CheckLock(c.Context(), lockRequest(holder.GetId(), "h1", "r3"))
	c.Assert(err, qt.ErrorMatches, `provider lock outcome is LOST: the lock's lease ended or another session took the lock`)
	c.Assert(checked.GetOutcome(), qt.Equals, protocol.LockResult_LOST)
	execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: holder.GetId(), RequestId: "e1",
		Units: []*protocol.Unit{{ChangeId: "1", Body: &protocol.Unit_Sql{Sql: `CREATE TABLE fenced (id bigint PRIMARY KEY)`}}}})
	c.Assert(err, qt.ErrorMatches, `provider execution: NOT_APPLIED \(0 units confirmed applied\): the session lost its lock before unit 1 of 1`)
	c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
}

// A read leaves the lock table out of the tables it describes and reserves it,
// so the host neither plans its removal nor writes it into an export.
func TestLocksSubprocess_ReadReservesTheLockTable(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	session := openSession(c, client, address)
	c.Assert(tryLock(c, client, session.GetId(), "l1", "r1"), qt.Equals, protocol.LockResult_ACQUIRED)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.GetId()})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.GetTables(), qt.HasLen, 0)
	c.Assert(snapshot.GetReservedObjects(), qt.HasLen, 1)
	c.Assert(snapshot.GetReservedObjects()[0].GetObject().GetName(), qt.Equals, "ptah_locks")
	c.Assert(snapshot.GetReservedObjects()[0].GetPurpose(), qt.Equals, protocol.ReservedObject_LOCK)
}

// leaseEnd reads when the lease of the lock row ends.
func leaseEnd(c *qt.C, address string) string {
	c.Helper()
	rows := queryRows(c, address, `SELECT expires_at::text FROM ptah_locks WHERE lock_name = 'ptah_schema_apply'`)
	c.Assert(rows, qt.HasLen, 1)
	return rows[0]
}

// A check that finds most of the lease left reads the lock row and writes
// nothing, and one that finds little left renews the lease. Spanner's
// emulator runs one read-write transaction at a time, so a check every
// second that wrote the row would stall every transaction the session runs
// beside it. Measured on the emulator: a two-statement migration took 5.8 s
// with every check writing and 0.75 s with checks that read, and with every
// check writing two of the twenty-two commands of the measurement failed, one
// of them losing its lock.
func TestLocksSubprocess_ChecksReadALiveLease(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	session := openSession(c, client, address)
	c.Assert(tryLock(c, client, session.GetId(), "l1", "r1"), qt.Equals, protocol.LockResult_ACQUIRED)
	taken := leaseEnd(c, address)
	checked, err := client.CheckLock(c.Context(), lockRequest(session.GetId(), "l1", "r2"))
	c.Assert(err, qt.IsNil)
	c.Assert(checked.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)
	c.Assert(leaseEnd(c, address), qt.Equals, taken)

	execSQL(c, address, `UPDATE ptah_locks SET expires_at = CURRENT_TIMESTAMP + INTERVAL '5 seconds' WHERE lock_name = 'ptah_schema_apply'`)
	short := leaseEnd(c, address)
	checked, err = client.CheckLock(c.Context(), lockRequest(session.GetId(), "l1", "r3"))
	c.Assert(err, qt.IsNil)
	c.Assert(checked.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)
	c.Assert(leaseEnd(c, address) > short, qt.IsTrue)
}
