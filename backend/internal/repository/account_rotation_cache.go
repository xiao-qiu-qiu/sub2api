package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	accountRotationKeyPrefix        = "concurrency:rotation:account:"
	accountRotationPoolPrefix       = "concurrency:rotation:pool:"
	accountRotationWaiterPrefix     = "concurrency:rotation:waiters:"
	accountRotationWaiterTTLSeconds = 60
)

var _ service.AccountRotationCache = (*concurrencyCache)(nil)
var _ service.AccountRotationHoldCache = (*concurrencyCache)(nil)

// Every operation uses the same Redis clock and state machine, including slot
// removal. In particular, there is no release -> check gap in which a new sticky
// request could slip into a drained account. State hashes deliberately have no
// TTL; request leases and the disposable pool planning index have separate lives.
//
// The bounded greedy planner examines at most 128 neighboring rest intervals and
// 49 candidate deadlines inside work +/- stagger%. It minimizes (overlap peak,
// integral of overlap seconds, distance from target + account hash jitter).
// A sweep of sorted interval endpoints makes scoring O(49*128), after sorting;
// Redis reads and expired-index cleanup are each bounded at 128 entries. This is
// a local forecast, not a global optimum: large pools use the earliest 128 rest
// ends near the candidate window, and long drains can move the actual rest later.
// The duration is sampled once per work cycle and reused when draining ends.
var accountRotationScript = redis.NewScript(`
redis.replicate_commands()
local stateKey, regularKey, liveKey, waiterKey = KEYS[1], KEYS[2], KEYS[3], KEYS[5]
local op, ttl, liveTTL = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3])
-- Project the exact admission state machine without changing leases or clocks.
local readOnly = op == 'peek'
if readOnly then op = 'check' end
local accountID, requestID = ARGV[4], ARGV[5]
local time = redis.call('TIME')
local now = tonumber(time[1])
local nowMS = now * 1000 + math.floor(tonumber(time[2]) / 1000)
local idleResetSeconds, scanLimit, candidateLimit = 300, 128, 49
local waiterTTL = 60
local poolPrefix = 'concurrency:rotation:pool:'
local cleanedPools = {}
local values = redis.call('HGETALL', stateKey)
local s = nil
if #values > 0 then
    s = {}
    for i = 1, #values, 2 do s[values[i]] = values[i + 1] end
    for _, field in ipairs({'started_at', 'drain_at', 'rest_until', 'idle_since',
        'rest_seconds', 'work_seconds', 'rest_min_seconds', 'rest_max_seconds',
        'stagger_percent', 'cycle', 'lease_until', 'planned_start', 'planned_end',
        'early_rest_enabled', 'hold', 'scheduled_rest_seconds', 'recovery_next_ms',
        'recovery_min_interval_ms', 'recovery_max_interval_ms'}) do
        s[field] = tonumber(s[field]) or 0
    end
    s.reason = s.reason or ''
    if s.recovery_min_interval_ms <= 0 then s.recovery_min_interval_ms = 500 end
    if s.recovery_max_interval_ms <= 0 then s.recovery_max_interval_ms = 2000 end
    if s.reason == '' and (s.phase == 'draining' or s.phase == 'resting') then s.reason = 'scheduled' end
end

local function forgetPlan()
    if not readOnly and s and s.pool and s.pool ~= '' then
        redis.call('ZREM', poolPrefix .. s.pool, stateKey)
    end
    if s then s.planned_start, s.planned_end = 0, 0 end
end

local function cleanupPlans(key)
    if cleanedPools[key] then return end
    cleanedPools[key] = true
    local expired = redis.call('ZRANGEBYSCORE', key, '-inf', now, 'LIMIT', 0, scanLimit)
    if not readOnly and #expired > 0 then redis.call('ZREM', key, unpack(expired)) end
end

local function indexPlan(startAt, endAt)
    s.planned_start, s.planned_end = startAt, endAt
    if s.pool ~= '' then
        local key = poolPrefix .. s.pool
        cleanupPlans(key)
        if not readOnly then redis.call('ZADD', key, endAt, stateKey) end
    end
end

local function hashNumber(suffix)
    -- A per-cycle hash of Redis TIME (including microseconds), account and
    -- generation avoids Redis Lua's repeatable math.random seed per invocation.
    return tonumber(string.sub(redis.sha1hex(stateKey .. ':' .. (s.revision or '')
        .. ':' .. s.cycle .. ':' .. time[1] .. ':' .. time[2] .. ':' .. suffix), 1, 8), 16)
end

local function planWindow()
    local target = s.started_at + s.work_seconds
    local spread = math.floor(s.work_seconds * s.stagger_percent / 100)
    local low, high = s.started_at + math.max(1, s.work_seconds - spread), target + spread
    local jitter = math.floor(spread / 5)
    local preferred = target + hashNumber('deadline') % (jitter * 2 + 1) - jitter
    local candidates, seen, boundaries, events = {}, {}, {}, {}
    local function addCandidate(value)
        value = math.floor(value)
        if value < low or value > high or seen[value] or #candidates >= candidateLimit then return end
        seen[value] = true
        candidates[#candidates + 1] = value
    end
    addCandidate(preferred)
    addCandidate(target)
    for i = 0, 24 do addCandidate(low + math.floor((high - low) * i / 24)) end

    if s.pool ~= '' then
        local key = poolPrefix .. s.pool
        cleanupPlans(key)
        -- Sorting by end skips plans already over before the earliest possible
        -- new rest. Read one bounded neighborhood; filter future starts below.
        local neighbors = redis.call('ZRANGEBYSCORE', key, '(' .. low, '+inf', 'LIMIT', 0, scanLimit)
        for _, member in ipairs(neighbors) do
            if member ~= stateKey then
                local plan = redis.call('HMGET', member, 'planned_start', 'planned_end', 'pool')
                local a, b = tonumber(plan[1]), tonumber(plan[2])
                if plan[3] == s.pool and a and b and b > low and a < high + s.rest_seconds then
                    events[#events + 1] = {a, 1}
                    events[#events + 1] = {b, -1}
                    for _, boundary in ipairs({a - s.rest_seconds, b, a, b - s.rest_seconds}) do
                        if boundary >= low and boundary <= high then boundaries[#boundaries + 1] = boundary end
                    end
                end
            end
        end
    end
    table.sort(boundaries, function(a, b)
        local da, db = math.abs(a - preferred), math.abs(b - preferred)
        if da == db then return a < b end
        return da < db
    end)
    for _, boundary in ipairs(boundaries) do
        addCandidate(boundary)
        if #candidates >= candidateLimit then break end
    end
    table.sort(events, function(a, b)
        if a[1] == b[1] then return a[2] < b[2] end
        return a[1] < b[1]
    end)
    local segments, depth, previous = {}, 0, nil
    for _, event in ipairs(events) do
        if previous and event[1] > previous and depth > 0 then
            segments[#segments + 1] = {previous, event[1], depth}
        end
        depth, previous = depth + event[2], event[1]
    end
    local best, bestPeak, bestOverlap, bestDistance = nil, nil, nil, nil
    for _, candidate in ipairs(candidates) do
        local peak, overlap = 1, 0
        local finish = candidate + s.rest_seconds
        for _, segment in ipairs(segments) do
            local duration = math.min(finish, segment[2]) - math.max(candidate, segment[1])
            if duration > 0 then
                peak = math.max(peak, segment[3] + 1)
                overlap = overlap + duration * segment[3]
            end
        end
        local distance = math.abs(candidate - preferred)
        if not best or peak < bestPeak or (peak == bestPeak and (overlap < bestOverlap
            or (overlap == bestOverlap and (distance < bestDistance
            or (distance == bestDistance and candidate < best))))) then
            best, bestPeak, bestOverlap, bestDistance = candidate, peak, overlap, distance
        end
    end
    s.drain_at = best
    indexPlan(best, best + s.rest_seconds)
end

local function clearWindow()
    forgetPlan()
    s.phase, s.started_at, s.drain_at, s.rest_until, s.rest_seconds = 'working', 0, 0, 0, 0
    s.reason, s.hold, s.scheduled_rest_seconds = '', 0, 0
end

local function applyPolicy()
    s.mode, s.work_seconds = ARGV[9], tonumber(ARGV[10])
    s.rest_min_seconds, s.rest_max_seconds = tonumber(ARGV[11]), tonumber(ARGV[12])
    s.stagger_percent, s.revision = tonumber(ARGV[13]), ARGV[14]
    s.early_rest_enabled = tonumber(ARGV[15]) or 0
    s.recovery_min_interval_ms = tonumber(ARGV[17]) or 500
    s.recovery_max_interval_ms = tonumber(ARGV[18]) or 2000
end

local function newPolicy()
    local cycle = s and s.cycle or 0
    forgetPlan()
    s = {phase = 'working', started_at = 0, drain_at = 0, rest_until = 0,
        idle_since = 0, rest_seconds = 0, cycle = cycle, lease_until = 0,
        planned_start = 0, planned_end = 0, pool = ARGV[8] or '',
        reason = '', hold = 0, scheduled_rest_seconds = 0, recovery_next_ms = 0}
    applyPolicy()
end

local function startWindow()
    if s.work_seconds <= 0 then return end
    s.cycle = s.cycle + 1
    s.phase, s.started_at, s.idle_since, s.rest_until = 'working', now, 0, 0
    s.rest_seconds = s.rest_min_seconds + hashNumber('rest') % (s.rest_max_seconds - s.rest_min_seconds + 1)
    planWindow()
end

local function save()
    if readOnly or not s then return end
    redis.call('HSET', stateKey,
        'phase', s.phase, 'started_at', s.started_at, 'drain_at', s.drain_at,
        'rest_until', s.rest_until, 'idle_since', s.idle_since, 'rest_seconds', s.rest_seconds,
        'mode', s.mode, 'work_seconds', s.work_seconds, 'rest_min_seconds', s.rest_min_seconds,
        'rest_max_seconds', s.rest_max_seconds, 'stagger_percent', s.stagger_percent,
        'revision', s.revision, 'pool', s.pool, 'cycle', s.cycle, 'lease_until', s.lease_until,
        'planned_start', s.planned_start, 'planned_end', s.planned_end,
        'reason', s.reason, 'hold', s.hold, 'early_rest_enabled', s.early_rest_enabled,
        'scheduled_rest_seconds', s.scheduled_rest_seconds,
        'recovery_next_ms', s.recovery_next_ms,
        'recovery_min_interval_ms', s.recovery_min_interval_ms,
        'recovery_max_interval_ms', s.recovery_max_interval_ms)
end

local function activeCount(key, expiry)
    if readOnly then return redis.call('ZCOUNT', key, '(' .. expiry, '+inf') end
    return redis.call('ZCARD', key)
end

local function snapshot()
    local phase = s and s.phase or 'ready'
    local recovering = s ~= nil and s.phase == 'working' and s.recovery_next_ms > 0
    if s and phase == 'working' and s.started_at == 0 then phase = 'ready' end
    local eligibleAt = 0
    if s and s.early_rest_enabled == 1 and s.work_seconds > 0 and s.started_at > 0 and s.drain_at > 0 then
        eligibleAt = math.max(s.started_at, s.drain_at - 1200)
    end
    return cjson.encode({phase = phase, started_at = s and s.started_at or 0,
        drain_at = s and s.drain_at or 0, rest_until = s and s.rest_until or 0,
        idle_since = s and s.idle_since or 0, rest_seconds = s and s.rest_seconds or 0,
        reason = s and s.reason or '', hold = s ~= nil and s.hold == 1,
        waiting_count = activeCount(waiterKey, now - waiterTTL), early_eligible_at = eligibleAt,
        recovering = recovering,
        retry_after_ms = recovering and math.max(0, s.recovery_next_ms - nowMS) or 0})
end

local function beginRecovery()
    -- Only the accumulated queue needs pacing. An idle account with no queue
    -- resumes normally. The first queued request may enter immediately.
    if activeCount(waiterKey, now - waiterTTL) > 0 then
        s.recovery_next_ms = math.max(s.recovery_next_ms, nowMS)
    end
end

-- Delete and inspect in one invocation, including user/key members for Live.
local removed = 0
if op == 'release' then
    removed = redis.call('ZREM', regularKey, requestID)
elseif op == 'release_live' then
    removed = redis.call('ZREM', liveKey, requestID)
    if #KEYS >= 7 then
        redis.call('ZREM', KEYS[6], requestID)
        redis.call('ZREM', KEYS[7], requestID)
    end
end
if not s and (op == 'release' or op == 'release_live') then return 1 end

if not readOnly then
    redis.call('ZREMRANGEBYSCORE', regularKey, '-inf', now - ttl)
    redis.call('ZREMRANGEBYSCORE', liveKey, '-inf', now - liveTTL)
    redis.call('ZREMRANGEBYSCORE', waiterKey, '-inf', now - waiterTTL)
end
if op == 'wait_refresh' then
    redis.call('ZADD', waiterKey, now, requestID)
    redis.call('EXPIRE', waiterKey, waiterTTL)
elseif op == 'wait_remove' then
    redis.call('ZREM', waiterKey, requestID)
end
-- Keep the last interval even after admitting the final waiter: a newcomer
-- must not slip through immediately behind it. Expired/canceled waiters do
-- not leave a permanent account-wide throttle.
if s and s.recovery_next_ms > 0 and nowMS >= s.recovery_next_ms
    and activeCount(waiterKey, now - waiterTTL) == 0 then
    s.recovery_next_ms = 0
end
local liveRefreshed = 0
if op == 'refresh_live' then
    -- Renew the joint lease and advance rotation in one invocation. A lone
    -- long-lived Live request must detect the early window on its heartbeat,
    -- without incoming requests or an administrator polling account status.
    local intact = redis.call('ZSCORE', liveKey, requestID) ~= false
    for _, key in ipairs({KEYS[6], KEYS[7]}) do
        redis.call('ZREMRANGEBYSCORE', key, '-inf', now - liveTTL)
        if redis.call('ZSCORE', key, requestID) == false then intact = false end
    end
    if intact then
        for _, key in ipairs({liveKey, KEYS[6], KEYS[7]}) do
            redis.call('ZADD', key, now, requestID)
            redis.call('EXPIRE', key, liveTTL)
        end
        liveRefreshed = 1
    end
end
local count = activeCount(regularKey, now - ttl) + activeCount(liveKey, now - liveTTL)
-- A manual action must not reset an automatic rest through a newer policy
-- revision, and a no-op cancel must not change an unrelated work window.
local noManualCancel = op == 'hold_cancel' and (not s or s.hold ~= 1 or s.reason ~= 'manual')
local automaticRestConflict = op == 'hold_start' and s and s.reason ~= 'manual'
    and (s.phase == 'draining' or (s.phase == 'resting' and now < s.rest_until))
if not noManualCancel and not automaticRestConflict
    and (op == 'check' or op == 'acquire' or op == 'hold_start' or op == 'hold_cancel') then
    if s and (s.revision ~= ARGV[14] or s.work_seconds ~= tonumber(ARGV[10])
        or s.early_rest_enabled ~= (tonumber(ARGV[15]) or 0)) then
        -- Configuration changes affect the next cycle, never release requests
        -- already waiting for a manually or opportunistically forced gap.
        if (s.hold == 1 and (s.phase == 'draining' or s.phase == 'resting'))
            or s.recovery_next_ms > 0 then applyPolicy()
        else newPolicy() end
    end
    if not s and (count > 0 or op == 'hold_start') then newPolicy() end
end

if op == 'refresh' and redis.call('ZSCORE', regularKey, requestID) ~= false then
    redis.call('ZADD', regularKey, 'XX', now, requestID)
    redis.call('EXPIRE', regularKey, ttl)
    redis.call('ZADD', KEYS[4], now + ttl, accountID)
end

local holdConflict = false
if s then
    if s.phase == 'resting' and now >= s.rest_until then
        clearWindow()
        s.idle_since = 0
        beginRecovery()
    end

    if count == 0 and s.idle_since == 0 then
        -- A real final release gives the exact zero-concurrency timestamp.
        -- For crashed leases, use the last observed expiry when available.
        s.idle_since = now
        if removed == 0 and s.lease_until > 0 then s.idle_since = math.min(now, s.lease_until) end
    end
    -- Prefer a completed natural break over a deadline first noticed much later.
    -- Clearing a window never starts another one; only positive concurrency does.
    if s.phase == 'working' and s.started_at > 0 and s.idle_since > 0
        and s.idle_since < s.drain_at and now - s.idle_since > idleResetSeconds then
        clearWindow()
    end

    if count > 0 then
        if s.phase == 'working' and s.started_at == 0 then startWindow() end
        s.idle_since = 0
        local regular = redis.call('ZREVRANGE', regularKey, 0, 0, 'WITHSCORES')
        local live = redis.call('ZREVRANGE', liveKey, 0, 0, 'WITHSCORES')
        s.lease_until = math.max(tonumber(regular[2] or '0') + ttl, tonumber(live[2] or '0') + liveTTL)
    end

    -- Check cannot know the caller's pool on first adoption of existing slots.
    -- Attach/replan once without resetting the observed work start. Later pool
    -- changes move the existing interval, and never extend its work deadline.
    if op == 'acquire' and s.pool ~= ARGV[8] then
        local firstPool = s.pool == ''
        forgetPlan()
        s.pool = ARGV[8]
        if s.work_seconds > 0 and s.started_at > 0 and s.phase == 'working' and firstPool and now < s.drain_at then
            planWindow()
        elseif s.started_at > 0 and s.rest_seconds > 0 then
            local startAt = s.phase == 'resting' and s.rest_until - s.rest_seconds or s.drain_at
            indexPlan(startAt, startAt + s.rest_seconds)
        end
    end
    if op == 'hold_start' then
        if s.hold == 1 and s.reason == 'manual' then
            -- Repeated clicks/retries cannot resample or extend this gap.
        elseif s.phase ~= 'working' or (s.work_seconds > 0 and s.started_at > 0 and now >= s.drain_at) then
            holdConflict = true
        else
            forgetPlan()
            s.scheduled_rest_seconds = s.rest_seconds
            s.phase, s.reason, s.hold = 'draining', 'manual', 1
            s.rest_seconds, s.rest_until = tonumber(ARGV[16]), 0
        end
    elseif op == 'hold_cancel' and s.hold == 1 and s.reason == 'manual' then
        forgetPlan()
        s.phase, s.reason, s.hold, s.rest_until = 'working', '', 0, 0
        s.rest_seconds, s.scheduled_rest_seconds = s.scheduled_rest_seconds, 0
        -- Cancellation did not complete a gap. Keep the original work clock
        -- and its sampled scheduled rest, including an already due deadline.
        -- Forced idle does not count as a natural break; start measuring any
        -- new natural idle period only after cancellation releases admission.
        s.idle_since = count == 0 and now or 0
        if s.work_seconds <= 0 then
            clearWindow()
        elseif s.started_at > 0 then
            indexPlan(s.drain_at, s.drain_at + s.rest_seconds)
        end
        if count > 0 and s.started_at == 0 then startWindow() end
        beginRecovery()
    end
    if s.phase == 'working' and s.work_seconds > 0 and s.started_at > 0 then
        if now >= s.drain_at then
            s.phase, s.reason, s.hold = 'draining', 'scheduled', 0
        elseif s.early_rest_enabled == 1 and now >= s.drain_at - 1200 and count < 2 then
            forgetPlan()
            s.scheduled_rest_seconds = s.rest_seconds
            s.phase, s.reason, s.hold = 'draining', 'early', 1
            s.rest_seconds, s.rest_until = 120, 0
        end
    end
    if s.phase == 'draining' then
        if count == 0 then
            -- Only a real check/release can persist the start of this rest.
            -- Re-projecting now + rest_seconds on every peek would keep a
            -- fallback router on its backup forever without advancing rotation.
            if readOnly then return redis.error_reply('ROUTING_ROTATION_TRANSITION_REQUIRED') end
            s.phase, s.rest_until = 'resting', now + s.rest_seconds
            indexPlan(now, s.rest_until)
        else
            indexPlan(now, now + s.rest_seconds)
        end
    end
end

if op == 'acquire' then
    -- A retry of an existing member is not a new slot, even during draining.
    local exists = redis.call('ZSCORE', regularKey, requestID) ~= false
    local allowed = not s or s.phase == 'working' or (s.phase == 'draining'
        and s.hold == 0 and s.mode == 'sticky' and ARGV[7] == '1' and count > 0)
    if s and s.recovery_next_ms > nowMS then allowed = false end
    if exists or (allowed and (tonumber(ARGV[6]) <= 0 or count < tonumber(ARGV[6]))) then
        redis.call('ZADD', regularKey, now, requestID)
        redis.call('EXPIRE', regularKey, ttl)
        redis.call('ZADD', KEYS[4], now + ttl, accountID)
        if not s then newPolicy() end
        redis.call('ZREM', waiterKey, requestID)
        if not exists and s.recovery_next_ms > 0 then
            -- Advance from the actual admission, not a missed deadline: spare
            -- concurrency or a delayed poll must never produce catch-up bursts.
            s.recovery_next_ms = nowMS + s.recovery_min_interval_ms
                + hashNumber('recovery:' .. requestID) % (s.recovery_max_interval_ms - s.recovery_min_interval_ms + 1)
        end
        if s.phase == 'working' and s.started_at == 0 then startWindow() end
        s.idle_since, s.lease_until = 0, math.max(s.lease_until, now + ttl)
        save()
        return {1, snapshot()}
    end
    if s and (s.hold == 1 or s.recovery_next_ms > 0) then
        -- Registration is atomic with rejection, so expiry/cancel cannot miss
        -- a request between admission and the service's waiter heartbeat.
        redis.call('ZADD', waiterKey, now, requestID)
        redis.call('EXPIRE', waiterKey, waiterTTL)
    end
    save()
    return {0, snapshot()}
end
save()
if op == 'check' then return snapshot() end
if op == 'hold_start' then return {holdConflict and -1 or 1, snapshot()} end
if op == 'hold_cancel' then return snapshot() end
if op == 'refresh_live' then return liveRefreshed end
return 1
`)

func accountRotationKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountRotationKeyPrefix, accountID)
}

func accountRotationPoolKey(pool string) string {
	return fmt.Sprintf("%s%x", accountRotationPoolPrefix, sha256.Sum256([]byte(pool)))
}

func accountRotationWaiterKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountRotationWaiterPrefix, accountID)
}

func accountRotationKeys(accountID int64) []string {
	return []string{accountRotationKey(accountID), accountSlotKey(accountID), liveAccountSlotKey(accountID), accountActiveIndexKey, accountRotationWaiterKey(accountID)}
}

func (c *concurrencyCache) accountRotationArgs(op string, accountID int64, requestID string, limit int, sticky bool, pool string, policy service.AccountRotationPolicy) []any {
	stickyArg := 0
	if sticky {
		stickyArg = 1
	}
	earlyArg := 0
	if policy.EarlyRestEnabled {
		earlyArg = 1
	}
	poolHash := ""
	if op == "acquire" {
		poolHash = fmt.Sprintf("%x", sha256.Sum256([]byte(pool)))
	}
	recoveryMin, recoveryMax := policy.RecoveryMinIntervalMS, policy.RecoveryMaxIntervalMS
	if recoveryMin == 0 && recoveryMax == 0 {
		recoveryMin, recoveryMax = 500, 2000
	}
	return []any{op, c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, requestID, limit, stickyArg, poolHash,
		policy.Mode, policy.WorkSeconds, policy.RestMinSeconds, policy.RestMaxSeconds, policy.StaggerPercent, policy.Revision, earlyArg,
		0, recoveryMin, recoveryMax}
}

func validateAccountRotationPolicy(policy service.AccountRotationPolicy) error {
	if (policy.RecoveryMinIntervalMS != 0 || policy.RecoveryMaxIntervalMS != 0) &&
		(policy.RecoveryMinIntervalMS < 1 || policy.RecoveryMaxIntervalMS < policy.RecoveryMinIntervalMS || policy.RecoveryMaxIntervalMS > 60000) {
		return fmt.Errorf("invalid account rotation recovery interval")
	}
	if (policy.Mode != "aggressive" && policy.Mode != "sticky") || policy.WorkSeconds < 0 ||
		(policy.WorkSeconds > 0 && (policy.RestMinSeconds <= 0 || policy.RestMaxSeconds < policy.RestMinSeconds)) ||
		policy.StaggerPercent < 0 || policy.StaggerPercent > 100 {
		return fmt.Errorf("invalid account rotation policy")
	}
	return nil
}

func decodeAccountRotationState(raw string) (*service.AccountRotationState, error) {
	var state service.AccountRotationState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("decode account rotation state: %w", err)
	}
	return &state, nil
}

func (c *concurrencyCache) CheckAccountRotation(ctx context.Context, accountID int64, policy service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	if err := validateAccountRotationPolicy(policy); err != nil {
		return nil, err
	}
	raw, err := accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		c.accountRotationArgs("check", accountID, "", 0, false, "", policy)...).Text()
	if err != nil {
		return nil, err
	}
	return decodeAccountRotationState(raw)
}

// CheckAccountRotationBatch is an optional scheduler optimization. Warm checks
// take one pipeline round trip and send only the SHA instead of one Lua source
// per candidate. A cold/flushed cache reloads once and retries only NOSCRIPTs.
func (c *concurrencyCache) CheckAccountRotationBatch(ctx context.Context, accountIDs []int64, policy service.AccountRotationPolicy) (map[int64]*service.AccountRotationState, error) {
	if err := validateAccountRotationPolicy(policy); err != nil {
		return nil, err
	}
	states := make(map[int64]*service.AccountRotationState, len(accountIDs))
	if len(accountIDs) == 0 {
		return states, nil
	}
	pipe := c.rdb.Pipeline()
	commands := make(map[int64]*redis.Cmd, len(accountIDs))
	for _, id := range accountIDs {
		if _, exists := commands[id]; !exists {
			commands[id] = accountRotationScript.EvalSha(ctx, pipe, accountRotationKeys(id),
				c.accountRotationArgs("check", id, "", 0, false, "", policy)...)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !redis.HasErrorPrefix(err, "NOSCRIPT") {
		return nil, err
	}
	var missing []int64
	for id, cmd := range commands {
		if redis.HasErrorPrefix(cmd.Err(), "NOSCRIPT") {
			missing = append(missing, id)
		} else if err := cmd.Err(); err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		if err := accountRotationScript.Load(ctx, c.rdb).Err(); err != nil {
			return nil, err
		}
		for _, id := range missing {
			commands[id] = accountRotationScript.EvalSha(ctx, pipe, accountRotationKeys(id),
				c.accountRotationArgs("check", id, "", 0, false, "", policy)...)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
	}
	for id, cmd := range commands {
		raw, err := cmd.Text()
		if err != nil {
			return nil, err
		}
		states[id], err = decodeAccountRotationState(raw)
		if err != nil {
			return nil, err
		}
	}
	return states, nil
}

func (c *concurrencyCache) AcquireAccountSlotWithRotation(ctx context.Context, accountID int64, maxConcurrency int, requestID string, policy service.AccountRotationPolicy, sticky bool, pool string) (bool, error) {
	acquired, _, err := c.AcquireAccountSlotWithRotationState(ctx, accountID, maxConcurrency, requestID, policy, sticky, pool)
	return acquired, err
}

// The admission decision and its snapshot come from one Redis invocation. A
// separate check could observe the gap ending and misclassify a held request as
// an ordinary capacity rejection, causing the caller to select another account.
func (c *concurrencyCache) AcquireAccountSlotWithRotationState(ctx context.Context, accountID int64, maxConcurrency int, requestID string, policy service.AccountRotationPolicy, sticky bool, pool string) (bool, *service.AccountRotationState, error) {
	if err := validateAccountRotationPolicy(policy); err != nil {
		return false, nil, err
	}
	acquired, state, err := c.runAccountRotationStatePair(ctx, accountID,
		c.accountRotationArgs("acquire", accountID, requestID, maxConcurrency, sticky, pool, policy)...)
	return acquired == 1, state, err
}

func (c *concurrencyCache) runAccountRotationStatePair(ctx context.Context, accountID int64, args ...any) (int64, *service.AccountRotationState, error) {
	values, err := accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID), args...).Slice()
	if err != nil {
		return 0, nil, err
	}
	if len(values) != 2 {
		return 0, nil, fmt.Errorf("invalid account rotation result length: %d", len(values))
	}
	result, ok := values[0].(int64)
	if !ok {
		return 0, nil, fmt.Errorf("invalid account rotation admission result: %T", values[0])
	}
	raw, ok := values[1].(string)
	if !ok {
		return 0, nil, fmt.Errorf("invalid account rotation snapshot: %T", values[1])
	}
	state, err := decodeAccountRotationState(raw)
	return result, state, err
}

func (c *concurrencyCache) SetAccountRotationHold(ctx context.Context, accountID int64, seconds int, policy service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	if err := validateAccountRotationPolicy(policy); err != nil {
		return nil, err
	}
	if seconds <= 0 {
		return nil, fmt.Errorf("account rest seconds must be positive")
	}
	args := c.accountRotationArgs("hold_start", accountID, "", 0, false, "", policy)
	args[15] = seconds
	result, state, err := c.runAccountRotationStatePair(ctx, accountID, args...)
	if err != nil {
		return nil, err
	}
	if result == -1 {
		return nil, infraerrors.Conflict("ACCOUNT_ALREADY_RESTING", "account is already draining or resting automatically")
	}
	return state, nil
}

func (c *concurrencyCache) CancelAccountRotationHold(ctx context.Context, accountID int64, policy service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	if err := validateAccountRotationPolicy(policy); err != nil {
		return nil, err
	}
	raw, err := accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		c.accountRotationArgs("hold_cancel", accountID, "", 0, false, "", policy)...).Text()
	if err != nil {
		return nil, err
	}
	return decodeAccountRotationState(raw)
}

func (c *concurrencyCache) RefreshAccountRotationWaiter(ctx context.Context, accountID int64, waiterID string) error {
	if waiterID == "" {
		return fmt.Errorf("account rest waiter ID must not be empty")
	}
	return accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		"wait_refresh", c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, waiterID).Err()
}

func (c *concurrencyCache) RemoveAccountRotationWaiter(ctx context.Context, accountID int64, waiterID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		"wait_remove", c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, waiterID).Err()
}

func (c *concurrencyCache) RefreshAccountRotationSlot(ctx context.Context, accountID int64, requestID string) error {
	return accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		"refresh", c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, requestID).Err()
}

func (c *concurrencyCache) refreshAccountRotationLiveLease(ctx context.Context, accountID, userID, apiKeyID int64, leaseID string) (bool, error) {
	keys := append(accountRotationKeys(accountID), liveUserSlotKey(userID), liveAPIKeySlotKey(apiKeyID))
	result, err := accountRotationScript.Run(ctx, c.rdb, keys,
		"refresh_live", c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, leaseID).Int()
	return result == 1, err
}

// For the Live migration path, release only this account's member while keeping
// the joint user/key lease. Its caller can use this instead of a standalone ZREM.
func (c *concurrencyCache) releaseAccountRotationLiveSlot(ctx context.Context, accountID int64, leaseID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		"release_live", c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, leaseID).Err()
}

func (c *concurrencyCache) releaseAccountRotationSlot(ctx context.Context, accountID int64, requestID string, liveUserID, liveAPIKeyID int64, live bool) error {
	// Forwarding cancellation cannot suppress the final distributed release.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	keys := accountRotationKeys(accountID)
	op := "release"
	if live {
		op = "release_live"
		keys = append(keys, liveUserSlotKey(liveUserID), liveAPIKeySlotKey(liveAPIKeyID))
	}
	if err := accountRotationScript.Run(ctx, c.rdb, keys, op, c.slotTTLSeconds,
		liveLeaseTTLSeconds, accountID, requestID).Err(); err != nil {
		return err
	}
	if !live {
		c.refreshAccountActiveIndex(ctx, accountID)
	}
	return nil
}
