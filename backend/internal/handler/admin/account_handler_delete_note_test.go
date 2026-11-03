package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type deleteNoteAdminService struct {
	*batchDeleteAdminService
	notes map[int64]string
}

func (s *deleteNoteAdminService) DeleteAccountWithNote(ctx context.Context, id int64, note string) error {
	s.mu.Lock()
	s.notes[id] = note
	s.mu.Unlock()
	return s.DeleteAccount(ctx, id)
}

func deleteNoteRouter(svc service.AdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.DELETE("/admin/accounts/:id", handler.Delete)
	router.POST("/admin/accounts/batch-delete", handler.BatchDelete)
	return router
}

func TestAccountHandlerDeleteOptionalNote(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantNote string
		status               int
	}{
		{name: "no body", status: http.StatusOK},
		{name: "no field", body: `{}`, status: http.StatusOK},
		{name: "empty note", body: `{"delete_note":""}`, status: http.StatusOK},
		{name: "trimmed empty", body: `{"delete_note":" \n\t "}`, status: http.StatusOK},
		{name: "trimmed note", body: `{"delete_note":"  暂停使用\n "}`, wantNote: "暂停使用", status: http.StatusOK},
		{name: "unicode character limit", body: `{"delete_note":"  ` + strings.Repeat("备", 1000) + `  "}`, wantNote: strings.Repeat("备", 1000), status: http.StatusOK},
		{name: "over limit", body: `{"delete_note":"` + strings.Repeat("备", 1001) + `"}`, status: http.StatusBadRequest},
		{name: "number", body: `{"delete_note":123}`, status: http.StatusBadRequest},
		{name: "object", body: `{"delete_note":{}}`, status: http.StatusBadRequest},
		{name: "array body", body: `[]`, status: http.StatusBadRequest},
		{name: "malformed body", body: `{"delete_note":`, status: http.StatusBadRequest},
		{name: "nul unsupported by postgres", body: `{"delete_note":"a\u0000b"}`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &batchDeleteAdminService{stubAdminService: newStubAdminService()}
			svc := &deleteNoteAdminService{batchDeleteAdminService: base, notes: make(map[int64]string)}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodDelete, "/admin/accounts/7", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			deleteNoteRouter(svc).ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.status != http.StatusOK {
				require.Empty(t, base.deletedIDs)
				require.Empty(t, svc.notes)
				return
			}
			require.Equal(t, []int64{7}, base.deletedIDs)
			require.Equal(t, tc.wantNote, svc.notes[7])
		})
	}
}

func TestAccountHandlerDeleteSupportsLegacyService(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"delete_note":" \n "}`} {
		svc := &batchDeleteAdminService{stubAdminService: newStubAdminService()}
		rec := httptest.NewRecorder()
		deleteNoteRouter(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/admin/accounts/9", strings.NewReader(body)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, []int64{9}, svc.deletedIDs)
	}
}

func TestAccountHandlerBatchDeleteSharedNote(t *testing.T) {
	parentID := int64(1)
	base := &batchDeleteAdminService{
		stubAdminService: newStubAdminService(),
		accountsByID: map[int64]*service.Account{
			1: {ID: 1},
			2: {ID: 2, ParentAccountID: &parentID},
			3: {ID: 3},
		},
	}
	svc := &deleteNoteAdminService{batchDeleteAdminService: base, notes: make(map[int64]string)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/batch-delete", strings.NewReader(`{"account_ids":[1,2,3,3],"delete_note":" 批量清理 \n"}`))
	deleteNoteRouter(svc).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, map[int64]string{1: "批量清理", 3: "批量清理"}, svc.notes)
	require.ElementsMatch(t, []int64{1, 3}, base.deletedIDs)
	var payload struct {
		Data struct {
			SuccessIDs []int64 `json:"success_ids"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, []int64{1, 2, 3}, payload.Data.SuccessIDs)
}

func TestAccountHandlerBatchDeleteRejectsInvalidNoteBeforeDeleting(t *testing.T) {
	for _, body := range []string{
		`{"account_ids":[1,2],"delete_note":"` + strings.Repeat("x", 1001) + `"}`,
		`{"account_ids":[1,2],"delete_note":false}`,
		`{"account_ids":[1,2],"delete_note":[]}`,
		`{"account_ids":[1,2],"delete_note":`,
	} {
		svc := &batchDeleteAdminService{stubAdminService: newStubAdminService()}
		rec := httptest.NewRecorder()
		deleteNoteRouter(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/batch-delete", strings.NewReader(body)))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Empty(t, svc.deletedIDs)
	}
}
