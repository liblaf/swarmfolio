package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestExecuteRecordsAcknowledgedMutations(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err != nil || report.Error != "" {
		t.Fatalf("error=%v report=%#v", err, report)
	}
	wantOperations := []string{"add", "delete", "start"}
	wantHashes := []string{qbt.addHash, "old", qbt.addHash}
	if len(report.Mutations) != len(wantOperations) {
		t.Fatalf("mutations=%#v", report.Mutations)
	}
	for index, mutation := range report.Mutations {
		if mutation.Operation != wantOperations[index] || !slices.Equal(mutation.Hashes, []string{wantHashes[index]}) || mutation.Status != "accepted" {
			t.Fatalf("mutation[%d]=%#v", index, mutation)
		}
		if mutation.Operation == "delete" {
			requireDeleteFiles(t, mutation, true)
		} else if mutation.DeleteFiles != nil {
			t.Fatalf("non-delete mutation has delete_files: %#v", mutation)
		}
	}
}

var errMutationResponse = errors.New("response lost after request")

type lostMutationResponseQBT struct {
	*fakeQBT
	operation string
}

func (q *lostMutationResponseQBT) Add(ctx context.Context, request qbittorrent.AddRequest) error {
	if err := q.fakeQBT.Add(ctx, request); err != nil {
		return err
	}
	if q.operation == "add" {
		return errMutationResponse
	}
	return nil
}

func (q *lostMutationResponseQBT) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	if err := q.fakeQBT.Delete(ctx, hashes, deleteFiles); err != nil {
		return err
	}
	if q.operation == "delete" {
		return errMutationResponse
	}
	return nil
}

func (q *lostMutationResponseQBT) Start(ctx context.Context, hashes []string) error {
	if err := q.fakeQBT.Start(ctx, hashes); err != nil {
		return err
	}
	if q.operation == "start" {
		return errMutationResponse
	}
	return nil
}

func TestExecuteRecordsUnconfirmedMutationsWithoutRetry(t *testing.T) {
	t.Parallel()
	for index, operation := range []string{"add", "delete", "start"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			runner := testRunner(qbt, mt)
			runner.QBittorrent = &lostMutationResponseQBT{fakeQBT: qbt, operation: operation}
			report, err := runner.Execute(context.Background(), true)
			if !errors.Is(err, errMutationResponse) || report.Error == "" || len(report.Mutations) != index+1 || qbt.mutations != index+1 || report.Actions[0].Applied {
				t.Fatalf("error=%v mutations=%d report=%#v", err, qbt.mutations, report)
			}
			last := report.Mutations[len(report.Mutations)-1]
			if last.Operation != operation || last.Status != "unconfirmed" {
				t.Fatalf("last mutation = %#v", last)
			}
			if operation == "delete" {
				requireDeleteFiles(t, last, true)
			} else if last.DeleteFiles != nil {
				t.Fatalf("non-delete mutation has delete_files: %#v", last)
			}
		})
	}
}

type failedRecoverySnapshotQBT struct {
	*fakeQBT
}

func (q *failedRecoverySnapshotQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if q.mutations > 0 {
		return nil, errMutationResponse
	}
	return q.fakeQBT.Torrents(ctx)
}

func TestExecuteRetainsMutationReceiptWhenRecoveryConfirmationFails(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"remove", "resume"} {
		t.Run(recovery, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			qbt = pendingQBT(qbt.addHash)
			if recovery == "remove" {
				mt.results = nil
			}
			runner := testRunner(qbt, mt)
			runner.QBittorrent = &failedRecoverySnapshotQBT{fakeQBT: qbt}
			report, err := runner.Execute(context.Background(), true)
			if !errors.Is(err, errMutationResponse) || report.Error == "" || len(report.Recoveries) != 0 || len(report.Mutations) != 1 || report.Mutations[0].Status != "accepted" {
				t.Fatalf("error=%v report=%#v", err, report)
			}
			if recovery == "remove" {
				requireDeleteFiles(t, report.Mutations[0], false)
			} else if report.Mutations[0].DeleteFiles != nil {
				t.Fatalf("resume mutation has delete_files: %#v", report.Mutations[0])
			}
		})
	}
}

func TestExecuteRecordsRollback(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.onAdd = func(*fakeQBT) { mt.results = nil }
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || len(report.Mutations) != 2 || report.Mutations[1].Operation != "delete" || report.Mutations[1].Status != "accepted" || !slices.Equal(report.Mutations[1].Hashes, []string{qbt.addHash}) {
		t.Fatalf("error=%v report=%#v", err, report)
	}
	requireDeleteFiles(t, report.Mutations[1], false)
	if len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "old" {
		t.Fatalf("rollback changed incumbent: %#v", qbt.torrents)
	}
}

func requireDeleteFiles(t *testing.T, mutation Mutation, want bool) {
	t.Helper()
	if mutation.DeleteFiles == nil || *mutation.DeleteFiles != want {
		t.Fatalf("mutation delete_files = %v, want %t: %#v", mutation.DeleteFiles, want, mutation)
	}
}
