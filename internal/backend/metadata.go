package backend

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/metadata"
	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
)

// maxReaderBytes bounds the records one metadata snapshot holds in memory. A
// larger store is refused rather than read in part.
const maxReaderBytes = 64 << 20

// readerLimit bounds the readers one session keeps. A closed reader stays to
// fence a late open of its ID.
const readerLimit = 1024

// reader is one snapshot of a metadata store, read at open and handed out a
// page at a time.
type reader struct {
	scope *protocol.MetadataScope
	// exists is false for a store that did not exist at open.
	exists  bool
	records []proto.Message
	// next is the page the next request must ask for, and cursor the first
	// record it returns.
	next   uint64
	cursor int
	// closed is set by a close, by a page that failed, and when the
	// transaction the reader reads in finishes.
	closed bool
}

// PrepareMetadata creates a missing log or tag table and checks an existing
// one holds exactly its layout. Inside a transaction it only checks an
// existing table, because Spanner creates a table outside transactions only.
func (p *Provider) PrepareMetadata(ctx context.Context, request *protocol.PrepareMetadataRequest) (*protocol.MetadataResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	answer := func(outcome protocol.MetadataResult_Outcome) *protocol.MetadataResult {
		return &protocol.MetadataResult{Scope: proto.CloneOf(request.GetScope()), RequestId: request.GetRequestId(), Outcome: outcome}
	}
	if !current.guard(ctx) {
		return answer(protocol.MetadataResult_REJECTED), nil
	}
	store := metadata.StoreOf(request.GetScope())
	exists, err := ownedtable.Locate(ctx, current.conn, store.Definition())
	switch {
	case err != nil:
		return answer(protocol.MetadataResult_REJECTED), nil
	case exists:
		return answer(protocol.MetadataResult_APPLIED), nil
	case request.GetScope().GetTransactionId() != "":
		return answer(protocol.MetadataResult_REJECTED), nil
	}
	for i, statement := range store.Statements() {
		if !current.guard(ctx) {
			if i == 0 {
				return answer(protocol.MetadataResult_REJECTED), nil
			}
			return answer(protocol.MetadataResult_UNKNOWN), nil
		}
		if _, err := current.conn.Exec(ctx, statement); err != nil {
			return answer(protocol.MetadataResult_UNKNOWN), nil
		}
	}
	if exists, err := ownedtable.Locate(ctx, current.conn, store.Definition()); err != nil || !exists {
		return answer(protocol.MetadataResult_UNKNOWN), nil
	}
	return answer(protocol.MetadataResult_APPLIED), nil
}

// MutateMetadata appends a log record, or records or removes a tag. A log
// record is one insert that commits as it runs; a tag changes in the
// request's transaction, or in one of the provider's own that it commits. A
// failure before a commit was sent wrote nothing; a commit Spanner did not
// confirm is UNKNOWN. A request is never sent twice.
func (p *Provider) MutateMetadata(ctx context.Context, request *protocol.MutateMetadataRequest) (*protocol.MetadataResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	answer := func(outcome protocol.MetadataResult_Outcome, deleted bool) *protocol.MetadataResult {
		return &protocol.MetadataResult{Scope: proto.CloneOf(request.GetScope()), RequestId: request.GetRequestId(), Outcome: outcome,
			Deleted: deleted && outcome == protocol.MetadataResult_APPLIED}
	}
	if !current.guard(ctx) {
		return answer(protocol.MetadataResult_REJECTED, false), nil
	}
	store := metadata.StoreOf(request.GetScope())
	exists, err := ownedtable.Locate(ctx, current.conn, store.Definition())
	switch {
	case err != nil:
		return answer(protocol.MetadataResult_REJECTED, false), nil
	case !exists && request.GetDeleteTag() != "":
		// A tag a missing table never held is absent already.
		return answer(protocol.MetadataResult_APPLIED, false), nil
	case !exists:
		return answer(protocol.MetadataResult_REJECTED, false), nil
	}
	if record := request.GetAppendLog(); record != nil {
		err := store.Append(ctx, current.conn, record)
		switch {
		case errors.Is(err, metadata.ErrDuplicate):
			return answer(protocol.MetadataResult_REJECTED, false), nil
		case err != nil:
			return answer(protocol.MetadataResult_UNKNOWN, false), nil
		}
		return answer(protocol.MetadataResult_APPLIED, false), nil
	}
	outcome, deleted := current.changeTag(ctx, store, request)
	return answer(outcome, deleted), nil
}

// changeTag records or removes the tag a request names, in the request's
// transaction or in one of the provider's own that it commits, and reports
// the outcome and whether a removed tag was held. The caller holds mu.
func (s *session) changeTag(ctx context.Context, store metadata.Store, request *protocol.MutateMetadataRequest) (protocol.MetadataResult_Outcome, bool) {
	change := func(q metadata.Querier) (bool, error) {
		if name := request.GetDeleteTag(); name != "" {
			return store.DeleteTag(ctx, q, name)
		}
		return false, store.PutTag(ctx, q, request.GetPutTag())
	}
	if id := request.GetScope().GetTransactionId(); id != "" {
		record, err := s.openTransaction(id)
		if err != nil {
			return protocol.MetadataResult_REJECTED, false
		}
		deleted, err := change(record.tx)
		if err != nil {
			// The transaction is aborted and commits nothing.
			record.failed = true
			return protocol.MetadataResult_REJECTED, false
		}
		return protocol.MetadataResult_APPLIED, deleted
	}
	tx, err := s.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return protocol.MetadataResult_REJECTED, false
	}
	// A rollback that fails leaves nothing written: no commit was sent.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	deleted, err := change(tx)
	if err != nil || !s.guard(ctx) {
		return protocol.MetadataResult_REJECTED, false
	}
	err = tx.Commit(ctx)
	switch refused, found := errors.AsType[*pgconn.PgError](err); {
	case err == nil:
		return protocol.MetadataResult_APPLIED, deleted
	case found && refused.Code == "40001":
		return protocol.MetadataResult_REJECTED, false
	default:
		return protocol.MetadataResult_UNKNOWN, false
	}
}

// LookupMetadataTag reads one tag, in the request's transaction when it
// names one. A missing table holds no tag, and the lookup creates nothing.
func (p *Provider) LookupMetadataTag(ctx context.Context, request *protocol.LookupMetadataTagRequest) (*protocol.MetadataTagResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	q, record, err := current.metadataQuerier(request.GetScope())
	if err != nil {
		return nil, err
	}
	store := metadata.StoreOf(request.GetScope())
	result := &protocol.MetadataTagResult{Scope: proto.CloneOf(request.GetScope()), RequestId: request.GetRequestId()}
	exists, err := current.locateMetadata(ctx, store)
	if err != nil || !exists {
		return result, err
	}
	result.Record, err = store.Lookup(ctx, q, request.GetName())
	if err != nil {
		if record != nil {
			record.failed = true
		}
		return nil, callFailed(ctx, "the provider could not read the tag table")
	}
	result.Found = result.Record != nil
	return result, nil
}

// OpenMetadataRead reads a snapshot of a log or tag table under a reader ID
// the caller chose, in the request's transaction when it names one. A
// missing table is a snapshot that says so, and opening creates nothing.
func (p *Provider) OpenMetadataRead(ctx context.Context, request *protocol.MetadataReadRequest) (*protocol.MetadataReadResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if found := current.readers[request.GetReaderId()]; found != nil {
		return nil, runtime.ErrorWithReason(codes.AlreadyExists, "the reader ID was used before")
	}
	if len(current.readers) >= readerLimit {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "the session used every reader it keeps; open another session")
	}
	q, record, err := current.metadataQuerier(request.GetScope())
	if err != nil {
		return nil, err
	}
	store := metadata.StoreOf(request.GetScope())
	exists, err := current.locateMetadata(ctx, store)
	if err != nil {
		return nil, err
	}
	opened := &reader{scope: proto.CloneOf(request.GetScope()), exists: exists}
	if exists {
		opened.records, err = store.Snapshot(ctx, q, maxReaderBytes)
		switch {
		case errors.Is(err, metadata.ErrTooLarge):
			return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "the table holds more records than a snapshot reads")
		case err != nil:
			if record != nil {
				record.failed = true
			}
			return nil, callFailed(ctx, "the provider could not read the table")
		}
	}
	if current.readers == nil {
		current.readers = make(map[string]*reader)
	}
	current.readers[request.GetReaderId()] = opened
	return &protocol.MetadataReadResult{Scope: proto.CloneOf(request.GetScope()), ReaderId: request.GetReaderId(), RequestId: request.GetRequestId()}, nil
}

// ReadMetadataPage hands out the next page of a snapshot: at most the
// request's count of records, within the message bound. Pages advance once
// each, from zero; a page that cannot be answered closes the reader.
func (p *Provider) ReadMetadataPage(ctx context.Context, request *protocol.ReadMetadataPageRequest) (*protocol.MetadataPage, error) {
	current, err := p.lookup(request.GetReader().GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	opened := current.readers[request.GetReader().GetReaderId()]
	switch {
	case opened == nil || opened.closed:
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the reader is not open")
	case !proto.Equal(opened.scope, request.GetReader().GetScope()):
		opened.closed = true
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the reader was opened for another store")
	case request.GetPage() != opened.next:
		opened.closed = true
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the page is not the reader's next page")
	}
	page := &protocol.MetadataPage{
		Reader: &protocol.MetadataReadResult{Scope: proto.CloneOf(opened.scope), ReaderId: request.GetReader().GetReaderId(), RequestId: request.GetReader().GetRequestId()},
		Page:   request.GetPage(), Exists: opened.exists,
	}
	for opened.cursor < len(opened.records) && len(page.GetLogs())+len(page.GetTags()) < int(request.GetMaxRecords()) {
		switch record := opened.records[opened.cursor].(type) {
		case *protocol.MigrationLogRecord:
			page.Logs = append(page.Logs, record)
		case *protocol.MigrationTagRecord:
			page.Tags = append(page.Tags, record)
		}
		if proto.Size(page) > provider.MaxMetadataBytes {
			opened.closed = true
			return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "one record is larger than a page carries")
		}
		opened.cursor++
	}
	page.Complete = opened.cursor == len(opened.records)
	opened.next++
	return page, nil
}

// CloseMetadataRead releases a reader. Closing an ID that was never opened
// fences it, so a late open with that ID reads nothing; closing twice
// succeeds.
func (p *Provider) CloseMetadataRead(_ context.Context, request *protocol.MetadataReadRequest) (*protocol.MetadataReadResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.readers == nil {
		current.readers = make(map[string]*reader)
	}
	if found := current.readers[request.GetReaderId()]; found != nil {
		found.closed, found.records = true, nil
	} else {
		current.readers[request.GetReaderId()] = &reader{scope: proto.CloneOf(request.GetScope()), closed: true}
	}
	return &protocol.MetadataReadResult{Scope: proto.CloneOf(request.GetScope()), ReaderId: request.GetReaderId(), RequestId: request.GetRequestId()}, nil
}

// metadataQuerier is where a metadata scope's reads run: in its transaction
// when it names one, on the session's connection otherwise. The caller holds
// mu.
func (s *session) metadataQuerier(scope *protocol.MetadataScope) (metadata.Querier, *transaction, error) {
	if id := scope.GetTransactionId(); id != "" {
		record, err := s.openTransaction(id)
		if err != nil {
			return nil, nil, err
		}
		return record.tx, record, nil
	}
	return s.conn, nil, nil
}

// locateMetadata reports whether a store's table exists and holds its layout,
// as an error the caller returns. The caller holds mu.
func (s *session) locateMetadata(ctx context.Context, store metadata.Store) (bool, error) {
	exists, err := ownedtable.Locate(ctx, s.conn, store.Definition())
	switch {
	case errors.Is(err, ownedtable.ErrLayout):
		return false, runtime.ErrorWithReason(codes.FailedPrecondition, "a table of the store's name exists and "+layoutReason(err))
	case err != nil:
		return false, callFailed(ctx, "the provider could not read the catalog")
	}
	return exists, nil
}

// closeReaders closes the readers that read in a transaction that finished.
// The caller holds mu.
func (s *session) closeReaders(transactionID string) {
	for _, opened := range s.readers {
		if opened.scope.GetTransactionId() == transactionID {
			opened.closed, opened.records = true, nil
		}
	}
}
