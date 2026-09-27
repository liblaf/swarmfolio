package app

import (
	"context"
	"slices"
)

func (r Runner) recordMutation(operation string, hashes []string, err error) {
	*r.mutations = append(*r.mutations, Mutation{Operation: operation, Hashes: slices.Clone(hashes), Status: mutationStatus(err)})
}

func (r Runner) delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	err := r.QBittorrent.Delete(ctx, hashes, deleteFiles)
	*r.mutations = append(*r.mutations, Mutation{
		Operation:   "delete",
		Hashes:      slices.Clone(hashes),
		Status:      mutationStatus(err),
		DeleteFiles: &deleteFiles,
	})
	return err
}

func (r Runner) start(ctx context.Context, hashes []string) error {
	err := r.QBittorrent.Start(ctx, hashes)
	r.recordMutation("start", hashes, err)
	return err
}

func mutationStatus(err error) string {
	if err != nil {
		return "unconfirmed"
	}
	return "accepted"
}
