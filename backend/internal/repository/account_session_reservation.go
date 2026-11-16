package repository

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	accountSessionReservationKeyPrefix = "concurrency:account_session:"
	accountSessionRegistryLimit        = 512
	accountSessionIdleLimit            = 64
	accountSessionIdleMilliseconds     = 60_000
	accountSessionStableMilliseconds   = 10_000
)

// The registry only describes load preference. It never grants, removes or
// refreshes the real Redis/SQL concurrency slots. Call start after admission and
// end when the admitted request releases its slot.
//
// Each account has six bounded keys: session deadlines, session metadata,
// request deadlines, request owners, idle deadlines and closed-request fences.
// All member deadlines use Redis TIME. An expired request is forgotten without
// creating idle capacity: its release was not observed.
var accountSessionReservationScript = redis.NewScript(`
	redis.replicate_commands()
	local sessions, states, active, owners, idle, closed = unpack(KEYS)
	local op = ARGV[1]
	-- Most schedulable accounts have no observed sessions. Keep their batch
	-- reads to one existence check instead of running the cleanup commands.
	if op == 'get' and redis.call('EXISTS', sessions, active) == 0 then return 0 end
	local request = ARGV[2]
	local session = ARGV[3]
	local capacity = math.min(tonumber(ARGV[4]), tonumber(ARGV[9]))
	local persistent = ARGV[5] == '1'
	local activeTTL = tonumber(ARGV[6]) * 1000
	local idleTTL = tonumber(ARGV[7])
	local stableAfter = tonumber(ARGV[8])
	local bound = tonumber(ARGV[10])
	local t = redis.call('TIME')
	local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
	local keyTTL = activeTTL + idleTTL
	local capacityField = '@capacity'
	local suspendedField = '@suspended_until'

	local function touch()
		for _, key in ipairs(KEYS) do
			if redis.call('EXISTS', key) == 1 then
				redis.call('PEXPIRE', key, keyTTL)
			end
		end
	end
	local function forgetSession(hash)
		redis.call('ZREM', sessions, hash)
		redis.call('HDEL', states, hash)
		redis.call('ZREM', idle, hash)
	end
	local function fence(member)
		redis.call('ZADD', closed, now + activeTTL, member)
		local surplus = redis.call('ZCARD', closed) - bound
		if surplus > 0 then redis.call('ZREMRANGEBYRANK', closed, 0, surplus - 1) end
	end
	local function trimIdle()
		local surplus = redis.call('ZCARD', idle) - capacity
		if surplus > 0 then redis.call('ZREMRANGEBYRANK', idle, 0, surplus - 1) end
	end

	-- Suspension prevents untracked requests at the registry's bound from
	-- turning into phantom idle reservations after the tracked requests end.
	local suspendedUntil = tonumber(redis.call('HGET', states, suspendedField) or '0')
	if suspendedUntil > now then
		if op == 'start' then
			redis.call('HSET', states, suspendedField, now + activeTTL)
		elseif op == 'end' then
			fence(request)
		end
		if op ~= 'get' then touch() end
		return 0
	elseif suspendedUntil > 0 then
		redis.call('HDEL', states, suspendedField)
	end

	-- Request and session indexes have independent bounds, so cleanup is
	-- limited to at most 512 members per index without SCAN or global state.
	local expiredRequests = redis.call('ZRANGEBYSCORE', active, '-inf', now)
	for _, member in ipairs(expiredRequests) do
		local hash = redis.call('HGET', owners, member)
		redis.call('ZREM', active, member)
		redis.call('HDEL', owners, member)
		fence(member)
		if hash then
			local raw = redis.call('HGET', states, hash)
			if raw then
				local state = cjson.decode(raw)
				state.active = state.active - 1
				-- A request that outlived its observation may still be
				-- running (for example after a real-slot heartbeat).
				-- Keep uncertainty through the session's quiet window so
				-- another turn cannot immediately turn it into idle load.
				state.uncertain = true
				if state.active <= 0 then
					state.active = 0
					redis.call('ZADD', sessions, now + idleTTL, hash)
				end
				redis.call('ZREM', idle, hash)
				redis.call('HSET', states, hash, cjson.encode(state))
			end
		end
	end
	for _, hash in ipairs(redis.call('ZRANGEBYSCORE', sessions, '-inf', now)) do
		forgetSession(hash)
	end
	redis.call('ZREMRANGEBYSCORE', idle, '-inf', now)
	redis.call('ZREMRANGEBYSCORE', closed, '-inf', now)
	if op == 'end' then
		capacity = tonumber(redis.call('HGET', states, capacityField) or '0')
	elseif redis.call('ZCARD', sessions) > 0 then
		redis.call('HSET', states, capacityField, capacity)
	end
	trimIdle()

	if op == 'get' then
		return redis.call('ZCARD', idle)
	end
	if op == 'start' then
		-- Poll/retry of a successful admission is not another turn, and must
		-- neither promote HTTP stability nor extend an idle/request lifetime.
		if redis.call('ZSCORE', closed, request) ~= false or
			redis.call('ZSCORE', active, request) ~= false then
			return 0
		end
		if redis.call('ZCARD', active) >= bound then
			redis.call('DEL', sessions, states, active, owners, idle)
			redis.call('HSET', states, suspendedField, now + activeTTL)
			touch()
			return 0
		end
		local raw = redis.call('HGET', states, session)
		if not raw and redis.call('ZCARD', sessions) >= bound then
			-- Prefer losing the oldest inactive observation over losing an
			-- active owner. Eviction only affects this advisory registry.
			for _, hash in ipairs(redis.call('ZRANGE', sessions, 0, -1)) do
				local candidate = redis.call('HGET', states, hash)
				if candidate and cjson.decode(candidate).active == 0 then
					forgetSession(hash)
					break
				end
			end
		end
		local state
		if raw then
			state = cjson.decode(raw)
			if persistent or now - state.first >= stableAfter then state.stable = true end
		else
			state = {first = now, stable = persistent, active = 0}
		end
		state.active = state.active + 1
		redis.call('HSET', states, capacityField, capacity, session, cjson.encode(state))
		redis.call('ZADD', sessions, now + activeTTL, session)
		redis.call('ZADD', active, now + activeTTL, request)
		redis.call('HSET', owners, request, session)
		redis.call('ZREM', idle, session)
	else
		local owner = redis.call('HGET', owners, request)
		-- A mismatched release cannot close someone else's admitted member.
		if owner and owner ~= session then return 0 end
		if not owner then
			if redis.call('ZSCORE', closed, request) == false then fence(request) end
			touch()
			return 0
		end
		redis.call('ZREM', active, request)
		redis.call('HDEL', owners, request)
		fence(request)
		local raw = redis.call('HGET', states, session)
		if raw then
			local state = cjson.decode(raw)
			state.active = state.active - 1
			redis.call('HSET', states, session, cjson.encode(state))
			if state.active <= 0 then
				redis.call('ZADD', sessions, now + idleTTL, session)
				if state.stable and not state.uncertain and capacity > 0 then
					redis.call('ZADD', idle, now + idleTTL, session)
					trimIdle()
				end
			end
		end
	end
	touch()
	return 0
`)

func accountSessionReservationKeys(accountID int64) []string {
	prefix := accountSessionReservationKeyPrefix + "{" + strconv.FormatInt(accountID, 10) + "}:"
	return []string{prefix + "sessions", prefix + "states", prefix + "active", prefix + "owners", prefix + "idle", prefix + "closed"}
}

func validAccountSessionHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (c *concurrencyCache) accountSessionReservationArgs(op, requestID, sessionHash string, maxConcurrency int, persistentRoute bool) []any {
	capacity := maxConcurrency / 2
	if capacity < 0 {
		capacity = 0
	}
	persistent := "0"
	if persistentRoute {
		persistent = "1"
	}
	return []any{op, requestID, strings.ToLower(sessionHash), capacity, persistent, c.slotTTLSeconds,
		accountSessionIdleMilliseconds, accountSessionStableMilliseconds, accountSessionIdleLimit, accountSessionRegistryLimit}
}

func (c *concurrencyCache) StartAccountSession(ctx context.Context, accountID int64, maxConcurrency int, requestID, sessionHash string, persistentRoute bool) error {
	if accountID <= 0 || requestID == "" || !validAccountSessionHash(sessionHash) {
		return nil
	}
	return accountSessionReservationScript.Run(ctx, c.rdb, accountSessionReservationKeys(accountID),
		c.accountSessionReservationArgs("start", requestID, sessionHash, maxConcurrency, persistentRoute)...).Err()
}

func (c *concurrencyCache) EndAccountSession(ctx context.Context, accountID int64, requestID, sessionHash string) error {
	if accountID <= 0 || requestID == "" || !validAccountSessionHash(sessionHash) {
		return nil
	}
	return accountSessionReservationScript.Run(ctx, c.rdb, accountSessionReservationKeys(accountID),
		c.accountSessionReservationArgs("end", requestID, sessionHash, 0, false)...).Err()
}

// GetAccountSessionReservations batches per-account cleanup/counting. A failed
// snapshot is returned as an error for the service to ignore as soft telemetry.
func (c *concurrencyCache) GetAccountSessionReservations(ctx context.Context, accounts []service.AccountWithConcurrency) (map[int64]int, error) {
	counts := make(map[int64]int, len(accounts))
	if len(accounts) == 0 {
		return counts, nil
	}
	pipe := c.rdb.Pipeline()
	commands := make(map[int64]*redis.Cmd, len(accounts))
	limits := make(map[int64]int, len(accounts))
	for _, account := range accounts {
		if _, exists := commands[account.ID]; exists {
			continue
		}
		limits[account.ID] = account.MaxConcurrency
		commands[account.ID] = accountSessionReservationScript.EvalSha(ctx, pipe, accountSessionReservationKeys(account.ID),
			c.accountSessionReservationArgs("get", "", "", account.MaxConcurrency, false)...)
	}
	if _, err := pipe.Exec(ctx); err != nil && !redis.HasErrorPrefix(err, "NOSCRIPT") {
		return nil, fmt.Errorf("get account session reservations: %w", err)
	}
	var missing []int64
	for id, cmd := range commands {
		if redis.HasErrorPrefix(cmd.Err(), "NOSCRIPT") {
			missing = append(missing, id)
		} else if err := cmd.Err(); err != nil {
			return nil, fmt.Errorf("get account session reservations: %w", err)
		}
	}
	if len(missing) > 0 {
		if err := accountSessionReservationScript.Load(ctx, c.rdb).Err(); err != nil {
			return nil, err
		}
		for _, id := range missing {
			commands[id] = accountSessionReservationScript.EvalSha(ctx, pipe, accountSessionReservationKeys(id),
				c.accountSessionReservationArgs("get", "", "", limits[id], false)...)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, fmt.Errorf("get account session reservations: %w", err)
		}
	}
	for id, cmd := range commands {
		count, err := cmd.Int()
		if err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, nil
}
