package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	accountRotationKeyPrefix  = "concurrency:rotation:account:"
	accountRotationPoolPrefix = "concurrency:rotation:pool:"
)

var _ service.AccountRotationCache = (*concurrencyCache)(nil)

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
local stateKey, regularKey, liveKey = KEYS[1], KEYS[2], KEYS[3]
local op, ttl, liveTTL = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3])
local accountID, requestID = ARGV[4], ARGV[5]
local time = redis.call('TIME')
local now = tonumber(time[1])
local idleResetSeconds, scanLimit, candidateLimit = 300, 128, 49
local poolPrefix = 'concurrency:rotation:pool:'
local cleanedPools = {}
local values = redis.call('HGETALL', stateKey)
local s = nil
if #values > 0 then
    s = {}
    for i = 1, #values, 2 do s[values[i]] = values[i + 1] end
    for _, field in ipairs({'started_at', 'drain_at', 'rest_until', 'idle_since',
        'rest_seconds', 'work_seconds', 'rest_min_seconds', 'rest_max_seconds',
        'stagger_percent', 'cycle', 'lease_until', 'planned_start', 'planned_end'}) do
        s[field] = tonumber(s[field]) or 0
    end
end

local function forgetPlan()
    if s and s.pool and s.pool ~= '' then
        redis.call('ZREM', poolPrefix .. s.pool, stateKey)
    end
    if s then s.planned_start, s.planned_end = 0, 0 end
end

local function cleanupPlans(key)
    if cleanedPools[key] then return end
    cleanedPools[key] = true
    local expired = redis.call('ZRANGEBYSCORE', key, '-inf', now, 'LIMIT', 0, scanLimit)
    if #expired > 0 then redis.call('ZREM', key, unpack(expired)) end
end

local function indexPlan(startAt, endAt)
    s.planned_start, s.planned_end = startAt, endAt
    if s.pool ~= '' then
        local key = poolPrefix .. s.pool
        cleanupPlans(key)
        redis.call('ZADD', key, endAt, stateKey)
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
end

local function newPolicy()
    local cycle = s and s.cycle or 0
    forgetPlan()
    s = {phase = 'working', started_at = 0, drain_at = 0, rest_until = 0,
        idle_since = 0, rest_seconds = 0, cycle = cycle, lease_until = 0,
        planned_start = 0, planned_end = 0, pool = ARGV[8] or '',
        mode = ARGV[9], work_seconds = tonumber(ARGV[10]),
        rest_min_seconds = tonumber(ARGV[11]), rest_max_seconds = tonumber(ARGV[12]),
        stagger_percent = tonumber(ARGV[13]), revision = ARGV[14]}
end

local function startWindow()
    s.cycle = s.cycle + 1
    s.phase, s.started_at, s.idle_since, s.rest_until = 'working', now, 0, 0
    s.rest_seconds = s.rest_min_seconds + hashNumber('rest') % (s.rest_max_seconds - s.rest_min_seconds + 1)
    planWindow()
end

local function save()
    if not s then return end
    redis.call('HSET', stateKey,
        'phase', s.phase, 'started_at', s.started_at, 'drain_at', s.drain_at,
        'rest_until', s.rest_until, 'idle_since', s.idle_since, 'rest_seconds', s.rest_seconds,
        'mode', s.mode, 'work_seconds', s.work_seconds, 'rest_min_seconds', s.rest_min_seconds,
        'rest_max_seconds', s.rest_max_seconds, 'stagger_percent', s.stagger_percent,
        'revision', s.revision, 'pool', s.pool, 'cycle', s.cycle, 'lease_until', s.lease_until,
        'planned_start', s.planned_start, 'planned_end', s.planned_end)
end

local function snapshot()
    local phase = s and s.phase or 'ready'
    if s and phase == 'working' and s.started_at == 0 then phase = 'ready' end
    return cjson.encode({phase = phase, started_at = s and s.started_at or 0,
        drain_at = s and s.drain_at or 0, rest_until = s and s.rest_until or 0,
        idle_since = s and s.idle_since or 0, rest_seconds = s and s.rest_seconds or 0})
end

-- Delete and inspect in one invocation, including user/key members for Live.
local removed = 0
if op == 'release' then
    removed = redis.call('ZREM', regularKey, requestID)
elseif op == 'release_live' then
    removed = redis.call('ZREM', liveKey, requestID)
    if #KEYS >= 6 then
        redis.call('ZREM', KEYS[5], requestID)
        redis.call('ZREM', KEYS[6], requestID)
    end
end
if not s and (op == 'release' or op == 'release_live') then return 1 end

redis.call('ZREMRANGEBYSCORE', regularKey, '-inf', now - ttl)
redis.call('ZREMRANGEBYSCORE', liveKey, '-inf', now - liveTTL)
local count = redis.call('ZCARD', regularKey) + redis.call('ZCARD', liveKey)
if op == 'check' or op == 'acquire' then
    if s and s.revision ~= ARGV[14] then newPolicy() end
    if not s and count > 0 then newPolicy() end
end

if op == 'refresh' and redis.call('ZSCORE', regularKey, requestID) ~= false then
    redis.call('ZADD', regularKey, 'XX', now, requestID)
    redis.call('EXPIRE', regularKey, ttl)
    redis.call('ZADD', KEYS[4], now + ttl, accountID)
end

if s then
    if s.phase == 'resting' and now >= s.rest_until then
        clearWindow()
        s.idle_since = 0
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
        if s.started_at > 0 and s.phase == 'working' and firstPool and now < s.drain_at then
            planWindow()
        elseif s.started_at > 0 then
            local startAt = s.phase == 'resting' and s.rest_until - s.rest_seconds or s.drain_at
            indexPlan(startAt, startAt + s.rest_seconds)
        end
    end
    if s.phase == 'working' and s.started_at > 0 and now >= s.drain_at then s.phase = 'draining' end
    if s.phase == 'draining' then
        if count == 0 then
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
        and s.mode == 'sticky' and ARGV[7] == '1' and count > 0)
    if exists or (allowed and (tonumber(ARGV[6]) <= 0 or count < tonumber(ARGV[6]))) then
        redis.call('ZADD', regularKey, now, requestID)
        redis.call('EXPIRE', regularKey, ttl)
        redis.call('ZADD', KEYS[4], now + ttl, accountID)
        if not s then newPolicy() end
        if s.started_at == 0 then startWindow() end
        s.idle_since, s.lease_until = 0, math.max(s.lease_until, now + ttl)
        save()
        return {1, now}
    end
    save()
    return {0, now}
end
save()
if op == 'check' then return snapshot() end
return 1
`)

func accountRotationKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountRotationKeyPrefix, accountID)
}

func accountRotationPoolKey(pool string) string {
	return fmt.Sprintf("%s%x", accountRotationPoolPrefix, sha256.Sum256([]byte(pool)))
}

func accountRotationKeys(accountID int64) []string {
	return []string{accountRotationKey(accountID), accountSlotKey(accountID), liveAccountSlotKey(accountID), accountActiveIndexKey}
}

func (c *concurrencyCache) accountRotationArgs(op string, accountID int64, requestID string, limit int, sticky bool, pool string, policy service.AccountRotationPolicy) []any {
	stickyArg := 0
	if sticky {
		stickyArg = 1
	}
	poolHash := ""
	if op == "acquire" {
		poolHash = fmt.Sprintf("%x", sha256.Sum256([]byte(pool)))
	}
	return []any{op, c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, requestID, limit, stickyArg, poolHash,
		policy.Mode, policy.WorkSeconds, policy.RestMinSeconds, policy.RestMaxSeconds, policy.StaggerPercent, policy.Revision}
}

func validateAccountRotationPolicy(policy service.AccountRotationPolicy) error {
	if (policy.Mode != "aggressive" && policy.Mode != "sticky") || policy.WorkSeconds <= 0 ||
		policy.RestMinSeconds <= 0 || policy.RestMaxSeconds < policy.RestMinSeconds ||
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
	if err := validateAccountRotationPolicy(policy); err != nil {
		return false, err
	}
	acquired, _, err := runScriptInt64Pair(ctx, c.rdb, accountRotationScript, accountRotationKeys(accountID),
		c.accountRotationArgs("acquire", accountID, requestID, maxConcurrency, sticky, pool, policy)...)
	return acquired == 1, err
}

func (c *concurrencyCache) RefreshAccountRotationSlot(ctx context.Context, accountID int64, requestID string) error {
	return accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		"refresh", c.slotTTLSeconds, liveLeaseTTLSeconds, accountID, requestID).Err()
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
