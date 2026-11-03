package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type deleteNoteLegacyRepo struct {
	AccountRepository
	deleted []int64
}

func (r *deleteNoteLegacyRepo) ListShadowsByParent(context.Context, int64) ([]*Account, error) {
	return []*Account{{ID: 2}}, nil
}

func (r *deleteNoteLegacyRepo) Delete(_ context.Context, id int64) error {
	r.deleted = append(r.deleted, id)
	return nil
}

type deleteNoteRepo struct {
	*deleteNoteLegacyRepo
	notes map[int64]string
	err   error
}

func (r *deleteNoteRepo) DeleteWithNote(_ context.Context, id int64, note string) error {
	r.notes[id] = note
	return r.err
}

func TestDeleteAccountWithNoteCascade(t *testing.T) {
	repo := &deleteNoteRepo{deleteNoteLegacyRepo: &deleteNoteLegacyRepo{}, notes: make(map[int64]string)}
	svc := &adminServiceImpl{accountRepo: repo}
	require.NoError(t, svc.DeleteAccountWithNote(context.Background(), 1, " \n 暂时停用 \t"))
	require.Equal(t, map[int64]string{1: "暂时停用", 2: "暂时停用"}, repo.notes)
	require.Empty(t, repo.deleted)

	repo.notes = make(map[int64]string)
	repo.err = errors.New("archive failed")
	require.ErrorIs(t, svc.DeleteAccountWithNote(context.Background(), 1, "备注"), repo.err)
	require.Equal(t, map[int64]string{2: "备注"}, repo.notes, "parent deletion must stop if archiving its shadow fails")
}

func TestDeleteAccountWithNoteLegacyRepository(t *testing.T) {
	for _, note := range []string{"", " \n\t "} {
		repo := &deleteNoteLegacyRepo{}
		svc := &adminServiceImpl{accountRepo: repo}
		require.NoError(t, svc.DeleteAccountWithNote(context.Background(), 1, note))
		require.Equal(t, []int64{2, 1}, repo.deleted)
	}
	repo := &deleteNoteLegacyRepo{}
	svc := &adminServiceImpl{accountRepo: repo}
	require.NoError(t, svc.DeleteAccount(context.Background(), 1))
	require.Equal(t, []int64{2, 1}, repo.deleted)
	repo.deleted = nil
	require.ErrorIs(t, svc.DeleteAccountWithNote(context.Background(), 1, "must not drop this"), ErrAccountTrashUnavailable)
	require.Empty(t, repo.deleted)
}

func TestDeleteAccountWithNoteValidation(t *testing.T) {
	// Validation must run before repository access, including for non-HTTP callers.
	svc := &adminServiceImpl{}
	for _, note := range []string{strings.Repeat("备", 1001), "a\x00b", string([]byte{0xff})} {
		require.Error(t, svc.DeleteAccountWithNote(context.Background(), 1, note))
	}
	repo := &deleteNoteRepo{deleteNoteLegacyRepo: &deleteNoteLegacyRepo{}, notes: make(map[int64]string)}
	svc.accountRepo = repo
	require.NoError(t, svc.DeleteAccountWithNote(context.Background(), 1, " \n"+strings.Repeat("备", 1000)+"\t "))
	require.Equal(t, strings.Repeat("备", 1000), repo.notes[1])
}
