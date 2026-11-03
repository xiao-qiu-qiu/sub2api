package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type accountManualRestAdminStub struct {
	service.AdminService
	account *service.Account
	err     error
	lookups []int64
}

func (s *accountManualRestAdminStub) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	s.lookups = append(s.lookups, id)
	return s.account, s.err
}

type accountManualRestCacheStub struct {
	service.ConcurrencyCache
	service.AccountRotationHoldCache
	state        *service.AccountRotationState
	err          error
	startIDs     []int64
	startSeconds []int
	cancelIDs    []int64
	policies     []service.AccountRotationPolicy
}

func (s *accountManualRestCacheStub) SetAccountRotationHold(_ context.Context, id int64, seconds int, policy service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	s.startIDs = append(s.startIDs, id)
	s.startSeconds = append(s.startSeconds, seconds)
	s.policies = append(s.policies, policy)
	return s.state, s.err
}

func (s *accountManualRestCacheStub) CancelAccountRotationHold(_ context.Context, id int64, _ service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	s.cancelIDs = append(s.cancelIDs, id)
	return s.state, s.err
}

func (s *accountManualRestCacheStub) CheckAccountRotation(context.Context, int64, service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	return s.state, s.err
}

func setupAccountManualRestRouter(t *testing.T) (*gin.Engine, *AccountHandler, *accountManualRestAdminStub, *accountManualRestCacheStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	admin := &accountManualRestAdminStub{account: &service.Account{ID: 101, Status: service.StatusActive, Schedulable: true}}
	cache := &accountManualRestCacheStub{state: &service.AccountRotationState{Phase: "draining", Reason: "manual", Hold: true, RestSeconds: 60}}
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountRotation(service.NewSettingService(&settingHandlerRepoStub{values: map[string]string{}}, nil), nil)
	h := &AccountHandler{adminService: admin, concurrencyService: concurrency}
	router := gin.New()
	router.POST("/accounts/:id/rotation/rest", h.StartAccountManualRest)
	router.DELETE("/accounts/:id/rotation/rest", h.CancelAccountManualRest)
	router.GET("/accounts/:id/rotation/rest", h.GetAccountRestState)
	return router, h, admin, cache
}

func TestAccountManualRestAdminReadStateDoesNotStartOrCancel(t *testing.T) {
	router, _, admin, cache := setupAccountManualRestRouter(t)
	cache.state.WaitingCount = 3
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts/101/rotation/rest", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, int64(3), gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation.waiting_count").Int())
	require.True(t, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation.hold").Bool())
	require.Empty(t, cache.startIDs)
	require.Empty(t, cache.cancelIDs)
	cache.state = &service.AccountRotationState{Phase: "ready"}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts/101/rotation/rest", nil))
	require.Equal(t, "null", gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation").Raw)
	admin.account = nil
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts/101/rotation/rest", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAccountManualRestAdminStartDefaultsAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body    string
		seconds int
	}{
		{"", 60}, {" \n\t ", 60}, {"{}", 60},
		{`{"seconds":1}`, 1}, {`{"seconds":17}`, 17}, {`{"seconds":60}`, 60},
		{`{"seconds":120}`, 120}, {`{"seconds":7199}`, 7199}, {`{"seconds":7200}`, 7200},
		{`{"minutes":1}`, 60}, {`{"minutes":17}`, 1020}, {`{"minutes":120}`, 7200},
	} {
		t.Run(tc.body+"/"+strconv.Itoa(tc.seconds), func(t *testing.T) {
			router, _, admin, cache := setupAccountManualRestRouter(t)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/101/rotation/rest", strings.NewReader(tc.body)))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, []int64{101}, admin.lookups)
			require.Equal(t, []int64{101}, cache.startIDs)
			require.Equal(t, []int{tc.seconds}, cache.startSeconds)
			require.Zero(t, cache.policies[0].WorkSeconds, "manual rest must work with automatic rotation disabled")
			require.Equal(t, "manual", gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation.reason").String())
			require.True(t, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation.hold").Bool())
			require.True(t, admin.account.Schedulable)
			require.Equal(t, service.StatusActive, admin.account.Status)
		})
	}
}

func TestAccountManualRestAdminRejectsInvalidDurationsBeforeMutation(t *testing.T) {
	for _, body := range []string{
		`{"seconds":0}`, `{"seconds":-1}`, `{"seconds":7201}`, `{"seconds":1.5}`, `{"seconds":1.0}`, `{"seconds":1e1}`,
		`{"seconds":"1"}`, `{"seconds":true}`, `{"seconds":null}`, `{"seconds":[]}`, `{"seconds":{}}`,
		`{"seconds":999999999999999999999999}`, `{"seconds":60,"minutes":1}`,
		`{"seconds":null,"minutes":1}`, `{"seconds":60,"minutes":null}`, `{"seconds":null,"minutes":null}`,
		`{"minutes":0}`, `{"minutes":-1}`, `{"minutes":121}`, `{"minutes":1.5}`,
		`{"minutes":1.0}`, `{"minutes":1e1}`, `{"minutes":{}}`,
		`{"minutes":"1"}`, `{"minutes":true}`, `{"minutes":null}`, `{"minutes":[]}`,
		`{"minutes":999999999999999999999999}`, `null`, `[]`, `1`, `"1"`, `{`,
		`{"minute":1}`, `{"seconds":1,"unknown":1}`, `{} {}`, `{} invalid`,
		`{"seconds":1} {"seconds":2}`, `{"seconds":1} null`,
	} {
		t.Run(body, func(t *testing.T) {
			router, _, admin, cache := setupAccountManualRestRouter(t)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/101/rotation/rest", strings.NewReader(body)))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Empty(t, admin.lookups)
			require.Empty(t, cache.startIDs)
		})
	}
}

func TestAccountManualRestAdminCancelAndInvalidIDs(t *testing.T) {
	router, _, admin, cache := setupAccountManualRestRouter(t)
	cache.state = &service.AccountRotationState{Phase: "working", StartedAt: 100, DrainAt: 18100}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/accounts/101/rotation/rest", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{101}, admin.lookups)
	require.Equal(t, []int64{101}, cache.cancelIDs)
	require.False(t, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation.hold").Bool())
	require.EqualValues(t, 100, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation.started_at").Int())
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, id := range []string{"0", "-1", "invalid", "999999999999999999999999"} {
			t.Run(method+"/"+id, func(t *testing.T) {
				router, _, admin, cache := setupAccountManualRestRouter(t)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, "/accounts/"+id+"/rotation/rest", strings.NewReader(`{}`)))
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Empty(t, admin.lookups)
				require.Empty(t, cache.startIDs)
				require.Empty(t, cache.cancelIDs)
			})
		}
	}
}

func TestAccountManualRestAdminMissingAccountAndLookupFailure(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, tc := range []struct {
			name string
			err  error
			code int
		}{{"missing", service.ErrAccountNotFound, http.StatusNotFound}, {"nil account", nil, http.StatusNotFound}, {"storage failure", errors.New("lookup unavailable"), http.StatusInternalServerError}} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				router, _, admin, cache := setupAccountManualRestRouter(t)
				admin.account, admin.err = nil, tc.err
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, "/accounts/101/rotation/rest", strings.NewReader(`{}`)))
				require.Equal(t, tc.code, rec.Code, rec.Body.String())
				require.Empty(t, cache.startIDs)
				require.Empty(t, cache.cancelIDs)
			})
		}
	}
}

func TestAccountManualRestAdminUnsupportedAndConflictErrors(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, missingService := range []bool{false, true} {
			t.Run(method+"/unsupported/"+strconv.FormatBool(missingService), func(t *testing.T) {
				router, h, _, cache := setupAccountManualRestRouter(t)
				h.concurrencyService = service.NewConcurrencyService(nil)
				if missingService {
					h.concurrencyService = nil
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, "/accounts/101/rotation/rest", strings.NewReader(`{}`)))
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				require.Equal(t, "ACCOUNT_ROTATION_UNSUPPORTED", gjson.GetBytes(rec.Body.Bytes(), "reason").String())
				require.Empty(t, cache.startIDs)
				require.Empty(t, cache.cancelIDs)
			})
		}
		t.Run(method+"/conflict", func(t *testing.T) {
			router, _, _, cache := setupAccountManualRestRouter(t)
			cache.err = infraerrors.Conflict("ACCOUNT_ROTATION_REST_CONFLICT", "account is already resting")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(method, "/accounts/101/rotation/rest", strings.NewReader(`{}`)))
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			require.Equal(t, "ACCOUNT_ROTATION_REST_CONFLICT", gjson.GetBytes(rec.Body.Bytes(), "reason").String())
		})
	}
}
