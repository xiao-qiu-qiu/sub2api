package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const stickySessionRevisionPrefix = "sticky_session_revision:"

func isReservedOpenAISessionOwnershipKey(sessionHash string) bool {
	return strings.HasPrefix(sessionHash, "openai:response:") ||
		strings.HasPrefix(sessionHash, "openai:http-response-owner:")
}

func buildSessionRevisionKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%s%d:%s", stickySessionRevisionPrefix, groupID, sessionHash)
}

func stickySessionRevisionKeyForOwnerKey(ownerKey string) string {
	return stickySessionRevisionPrefix + strings.TrimPrefix(ownerKey, stickySessionPrefix)
}

var setSessionAccountBindingScript = redis.NewScript(`
local ttl = tonumber(ARGV[3])
if ttl == -1 then
  ttl = redis.call('PTTL', KEYS[1])
end
if ttl > 0 then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ttl)
  redis.call('SET', KEYS[2], ARGV[2], 'PX', ttl)
else
  redis.call('SET', KEYS[1], ARGV[1])
  redis.call('SET', KEYS[2], ARGV[2])
end
return 1
`)

var refreshSessionAccountBindingScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('DEL', KEYS[2])
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[1])
return 1
`)

var clearSessionAccountBindingsScript = redis.NewScript(`
local deleted = 0
for i = 1, #KEYS, 2 do
  deleted = deleted + redis.call('DEL', KEYS[i])
  redis.call('DEL', KEYS[i + 1])
end
return deleted
`)

var readSessionAccountBindingScript = redis.NewScript(`
local owner = redis.call('GET', KEYS[1])
local revisionKey = KEYS[2]
if owner == false and #KEYS > 2 then
  owner = redis.call('GET', KEYS[3])
  revisionKey = KEYS[4]
end
if owner == false then
  return false
end
return {owner, redis.call('GET', revisionKey) or ''}
`)

var compareAndSwapSessionAccountIDScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
local revisionKey = KEYS[2]
if current == false and #KEYS > 2 then
  current = redis.call('GET', KEYS[3])
  revisionKey = KEYS[4]
end
if current == false or current ~= ARGV[1] or (redis.call('GET', revisionKey) or '') ~= ARGV[2] then
  return 0
end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[5])
redis.call('SET', KEYS[2], ARGV[4], 'PX', ARGV[5])
if #KEYS > 2 then
  redis.call('SET', KEYS[3], ARGV[3], 'PX', ARGV[6])
  redis.call('SET', KEYS[4], ARGV[4], 'PX', ARGV[6])
end
return 1
`)

func stickySessionBindingKeys(groupID int64, sessionHash, legacySessionHash string) []string {
	keys := []string{buildSessionKey(groupID, sessionHash), buildSessionRevisionKey(groupID, sessionHash)}
	if legacySessionHash != "" && legacySessionHash != sessionHash {
		keys = append(keys, buildSessionKey(groupID, legacySessionHash), buildSessionRevisionKey(groupID, legacySessionHash))
	}
	return keys
}

func (c *gatewayCache) ReadSessionAccountBinding(ctx context.Context, groupID int64, sessionHash string, legacySessionHash string) (int64, string, error) {
	if c == nil || c.rdb == nil {
		return 0, "", errors.New("gateway cache unavailable")
	}
	if strings.TrimSpace(sessionHash) == "" || (legacySessionHash != "" && strings.TrimSpace(legacySessionHash) == "") ||
		isReservedOpenAISessionOwnershipKey(sessionHash) || isReservedOpenAISessionOwnershipKey(legacySessionHash) {
		return 0, "", errors.New("invalid sticky session binding read")
	}
	values, err := readSessionAccountBindingScript.Run(ctx, c.rdb, stickySessionBindingKeys(groupID, sessionHash, legacySessionHash)).Slice()
	if errors.Is(err, redis.Nil) {
		return 0, "", service.ErrStickySessionNotFound
	}
	if err != nil {
		return 0, "", err
	}
	if len(values) != 2 {
		return 0, "", errors.New("invalid sticky session binding result")
	}
	ownerText, ownerOK := values[0].(string)
	revision, revisionOK := values[1].(string)
	owner, err := strconv.ParseInt(ownerText, 10, 64)
	if !ownerOK || !revisionOK || err != nil || owner <= 0 {
		return 0, "", errors.New("invalid sticky session binding record")
	}
	return owner, revision, nil
}

func (c *gatewayCache) CompareAndSwapSessionAccountID(ctx context.Context, groupID int64, sessionHash string, legacySessionHash string, expectedAccountID int64, expectedRevision string, accountID int64, ttl time.Duration, legacyTTL time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, errors.New("gateway cache unavailable")
	}
	if strings.TrimSpace(sessionHash) == "" || expectedAccountID <= 0 || accountID <= 0 || ttl.Milliseconds() <= 0 ||
		isReservedOpenAISessionOwnershipKey(sessionHash) || isReservedOpenAISessionOwnershipKey(legacySessionHash) {
		return false, errors.New("invalid sticky session migration")
	}
	if legacySessionHash != "" && legacySessionHash != sessionHash {
		if strings.TrimSpace(legacySessionHash) == "" || legacyTTL.Milliseconds() <= 0 {
			return false, errors.New("invalid legacy sticky session migration")
		}
	}
	revision, err := uuid.NewRandom()
	if err != nil {
		return false, err
	}
	n, err := compareAndSwapSessionAccountIDScript.Run(ctx, c.rdb, stickySessionBindingKeys(groupID, sessionHash, legacySessionHash), expectedAccountID, expectedRevision, accountID, revision.String(), ttl.Milliseconds(), legacyTTL.Milliseconds()).Int()
	return n == 1, err
}

var _ service.StickySessionMigrationCache = (*gatewayCache)(nil)
