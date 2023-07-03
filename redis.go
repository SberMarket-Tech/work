package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gomodule/redigo/redis"
)

func redisNamespacePrefix(namespace string) string {
	l := len(namespace)
	if (l > 0) && (namespace[l-1] != ':') {
		namespace = namespace + ":"
	}
	return namespace
}

func redisKeyKnownJobs(namespace string) string {
	return redisNamespacePrefix(namespace) + "known_jobs"
}

// returns "<namespace>:jobs:"
// so that we can just append the job name and be good to go
func redisKeyJobsPrefix(namespace string) string {
	return redisNamespacePrefix(namespace) + "jobs:"
}

func redisKeyJobs(namespace, jobName string) string {
	return redisKeyJobsPrefix(namespace) + jobName
}

func redisJobNameFromKey(namespace, key string) string {
	return strings.TrimPrefix(key, redisKeyJobsPrefix(namespace))
}

func redisKeyJobsInProgress(namespace, poolID, jobName string) string {
	return fmt.Sprintf("%s:%s:inprogress", redisKeyJobs(namespace, jobName), poolID)
}

func redisKeyRetry(namespace string) string {
	return redisNamespacePrefix(namespace) + "retry"
}

func redisKeyDead(namespace string) string {
	return redisNamespacePrefix(namespace) + "dead"
}

func redisKeyScheduled(namespace string) string {
	return redisNamespacePrefix(namespace) + "scheduled"
}

func redisKeyWorkerObservation(namespace, workerID string) string {
	return redisNamespacePrefix(namespace) + "worker:" + workerID
}

func redisKeyWorkerPools(namespace string) string {
	return redisNamespacePrefix(namespace) + "worker_pools"
}

func redisKeyHeartbeat(namespace, workerPoolID string) string {
	return redisNamespacePrefix(namespace) + "worker_pools:" + workerPoolID
}

func redisKeyJobsPaused(namespace, jobName string) string {
	return redisKeyJobs(namespace, jobName) + ":paused"
}

func redisKeyJobsLock(namespace, jobName string) string {
	return redisKeyJobs(namespace, jobName) + ":lock"
}

func redisKeyJobsLockInfo(namespace, jobName string) string {
	return redisKeyJobs(namespace, jobName) + ":lock_info"
}

func redisJobNameFromLockInfoKey(namespace, key string) string {
	return redisJobNameFromKey(namespace, strings.TrimSuffix(key, ":lock_info"))
}

func redisKeyJobsConcurrency(namespace, jobName string) string {
	return redisKeyJobs(namespace, jobName) + ":max_concurrency"
}

func redisKeyUniqueJob(namespace, jobName string, args map[string]interface{}) (string, error) {
	var buf bytes.Buffer

	buf.WriteString(redisNamespacePrefix(namespace))
	buf.WriteString("unique:")
	buf.WriteString(jobName)
	buf.WriteRune(':')

	if args != nil {
		err := json.NewEncoder(&buf).Encode(args)
		if err != nil {
			return "", err
		}
	}

	return buf.String(), nil
}

func redisKeyLastPeriodicEnqueue(namespace string) string {
	return redisNamespacePrefix(namespace) + "last_periodic_enqueue"
}

func redisKeyReaperLock(namespace string) string {
	return redisNamespacePrefix(namespace) + "reaper_lock"
}

// Used to fetch the next job to run
//
// KEYS[1] = the 1st job queue we want to try, eg, "work:jobs:emails"
// KEYS[2] = the 1st job queue's in prog queue, eg, "work:jobs:emails:97c84119d13cb54119a38743:inprogress"
// KEYS[3] = the 2nd job queue...
// KEYS[4] = the 2nd job queue's in prog queue...
// ...
// KEYS[N] = the last job queue...
// KEYS[N+1] = the last job queue's in prog queue...
// ARGV[1] = job queue's workerPoolID
var redisLuaFetchJob = fmt.Sprintf(`
local function acquireLock(lockKey, lockInfoKey, workerPoolID)
  redis.call('incr', lockKey)
  redis.call('hincrby', lockInfoKey, workerPoolID, 1)
end

local function haveJobs(jobQueue)
  return redis.call('llen', jobQueue) > 0
end

local function isPaused(pauseKey)
  return redis.call('get', pauseKey)
end

local function canRun(lockKey, maxConcurrency)
  local activeJobs = tonumber(redis.call('get', lockKey))
  if (not maxConcurrency or maxConcurrency == 0) or (not activeJobs or activeJobs < maxConcurrency) then
    -- default case: maxConcurrency not defined or set to 0 means no cap on concurrent jobs OR
    -- maxConcurrency set, but lock does not yet exist OR
    -- maxConcurrency set, lock is set, but not yet at max concurrency
    return true
  else
    -- we are at max capacity for running jobs
    return false
  end
end

local res, jobQueue, inProgQueue, pauseKey, lockKey, maxConcurrency, workerPoolID, concurrencyKey, lockInfoKey
local keylen = #KEYS
workerPoolID = ARGV[1]

for i=1,keylen,%d do
  jobQueue = KEYS[i]
  inProgQueue = KEYS[i+1]
  pauseKey = KEYS[i+2]
  lockKey = KEYS[i+3]
  lockInfoKey = KEYS[i+4]
  concurrencyKey = KEYS[i+5]

  maxConcurrency = tonumber(redis.call('get', concurrencyKey))

  if haveJobs(jobQueue) and not isPaused(pauseKey) and canRun(lockKey, maxConcurrency) then
    acquireLock(lockKey, lockInfoKey, workerPoolID)
    res = redis.call('rpoplpush', jobQueue, inProgQueue)
    return {res, jobQueue, inProgQueue}
  end
end
return nil`, fetchKeysPerJobType)

// Used to remove job from the in-progress queue.
//
// KEYS[1] = in-progress job queue
// KEYS[2] = job's lock key
// KEYS[3] = job's lock info key
// KEYS[4] = forward queue
// ARGV[1] = worker pool id
// ARGV[2] = job value
// ARGV[3] = should the failed job be redirected to another queue?
// ARGV[4] = failed job score
// ARGV[5] = failed job value
var redisRemoveJobFromInProgress = redis.NewScript(4, `
local function releaseLock(lockKey, lockInfoKey, workerPoolID)
  redis.call('decr', lockKey)
  redis.call('hincrby', lockInfoKey, workerPoolID, -1)
end

local inProgQueue = KEYS[1]
local lockKey = KEYS[2]
local lockInfoKey = KEYS[3]
local workerPoolID = ARGV[1]
local job = ARGV[2]
local forward = ARGV[3]
local result = tonumber(redis.call('lrem', inProgQueue, 1, job))

if result ~= 0 then
  releaseLock(lockKey, lockInfoKey, workerPoolID)

  if forward then
    local queue = KEYS[4]
    local score = ARGV[4]
    local failedJob = ARGV[5]

    redis.call('zadd', queue, score, failedJob)
  end
end

return nil
`)

// Used by the reaper to re-enqueue jobs that were in progress
//
// KEYS[1] = the 1st job's in progress queue
// KEYS[2] = the 1st job's job queue
// KEYS[3] = the 1nd job's lock key
// KEYS[4] = the 1nd job's lock info key
// KEYS[5] = the 2st job's in progress queue
// KEYS[6] = the 2st job's job queue
// KEYS[7] = the 2nd job's lock key
// KEYS[8] = the 2nd job's lock info key
// ...
// KEYS[N] = the last job's in progress queue
// KEYS[N+1] = the last job's job queue
// KEYS[N+2] = the last job's lock key
// KEYS[N+3] = the last job's lock info key
// ARGV[1] = workerPoolID for job queue
var redisLuaReenqueueJob = fmt.Sprintf(`
local function releaseLock(lockKey, lockInfoKey, workerPoolID)
  redis.call('decr', lockKey)
  redis.call('hincrby', lockInfoKey, workerPoolID, -1)
end

local keylen = #KEYS
local res, jobQueue, inProgQueue, workerPoolID, lockKey, lockInfoKey
workerPoolID = ARGV[1]

for i=1,keylen,%d do
  inProgQueue = KEYS[i]
  jobQueue = KEYS[i+1]
  lockKey = KEYS[i+2]
  lockInfoKey = KEYS[i+3]
  res = redis.call('rpoplpush', inProgQueue, jobQueue)
  if res then
    releaseLock(lockKey, lockInfoKey, workerPoolID)
    return {res, inProgQueue, jobQueue}
  end
end
return nil`, requeueKeysPerJob)

// Used by the reaper to clean up stale locks
//
// KEYS[1] = the 1st job's lock
// KEYS[2] = the 1st job's lock info hash
// KEYS[3] = the 2nd job's lock
// KEYS[4] = the 2nd job's lock info hash
// ...
// KEYS[N] = the last job's lock
// KEYS[N+1] = the last job's lock info haash
// ARGV[1] = the dead worker pool id
var redisLuaReapStaleLocks = `
local keylen = #KEYS
local lock, lockInfo, deadLockCount
local deadPoolID = ARGV[1]
local negativeLocks = {}

for i=1,keylen,2 do
  lock = KEYS[i]
  lockInfo = KEYS[i+1]
  deadLockCount = tonumber(redis.call('hget', lockInfo, deadPoolID))

  if deadLockCount then
    redis.call('decrby', lock, deadLockCount)
    redis.call('hdel', lockInfo, deadPoolID)

    if tonumber(redis.call('get', lock)) < 0 then
      table.insert(negativeLocks, lock)
      redis.call('set', lock, 0)
    end
  end
end

return negativeLocks
`

// KEYS[1] = zset of jobs (retry or scheduled), eg work:retry
// KEYS[2] = zset of dead, eg work:dead. If we don't know the jobName of a job, we'll put it in dead.
// KEYS[3...] = known job queues, eg ["work:jobs:create_watch", "work:jobs:send_email", ...]
// ARGV[1] = jobs prefix, eg, "work:jobs:". We'll take that and append the job name from the JSON object in order to queue up a job
// ARGV[2] = current time in epoch seconds
var redisLuaZremLpushCmd = `
local res, j, queue
local nowTs = tonumber(ARGV[2])

res = redis.call('zrangebyscore', KEYS[1], '-inf', ARGV[2], 'LIMIT', 0, 1)

if #res > 0 then
  j = cjson.decode(res[1])
  redis.call('zrem', KEYS[1], res[1])
  queue = ARGV[1] .. j['name']

  for _,v in pairs(KEYS) do
    if v == queue then
      -- If for some reason (e.g., the service was offline) the periodic job was
      -- not executed, skip the execution.
      if j['d'] ~= nil and nowTs > j['d'] then
        return 'ok'
      end

      j['t'] = nowTs
      redis.call('lpush', queue, cjson.encode(j))

      return 'ok'
    end
  end

  j['err'] = 'unknown job when requeueing'
  j['failed_at'] = nowTs
  redis.call('zadd', KEYS[2], ARGV[2], cjson.encode(j))

  return 'dead' -- put on dead queue
end

return nil
`

// KEYS[1] = zset of (dead|scheduled|retry), eg, work:dead
// ARGV[1] = died at. The z rank of the job.
// ARGV[2] = job ID to requeue
// Returns:
// - number of jobs deleted (typically 1 or 0)
// - job bytes (last job only)
var redisLuaDeleteSingleCmd = `
local jobs, i, j, deletedCount, jobBytes
jobs = redis.call('zrangebyscore', KEYS[1], ARGV[1], ARGV[1])
local jobCount = #jobs
jobBytes = ''
deletedCount = 0
for i=1,jobCount do
  j = cjson.decode(jobs[i])
  if j['id'] == ARGV[2] then
    redis.call('zrem', KEYS[1], jobs[i])
    deletedCount = deletedCount + 1
    jobBytes = jobs[i]
  end
end
return {deletedCount, jobBytes}
`

// KEYS[1] = zset of dead jobs, eg, work:dead
// KEYS[2...] = known job queues, eg ["work:jobs:create_watch", "work:jobs:send_email", ...]
// ARGV[1] = jobs prefix, eg, "work:jobs:". We'll take that and append the job name from the JSON object in order to queue up a job
// ARGV[2] = current time in epoch seconds
// ARGV[3] = died at. The z rank of the job.
// ARGV[4] = job ID to requeue
// Returns: number of jobs requeued (typically 1 or 0)
var redisLuaRequeueSingleDeadCmd = `
local jobs, i, j, queue, found, requeuedCount
jobs = redis.call('zrangebyscore', KEYS[1], ARGV[3], ARGV[3])
local jobCount = #jobs
requeuedCount = 0
for i=1,jobCount do
  j = cjson.decode(jobs[i])
  if j['id'] == ARGV[4] then
    redis.call('zrem', KEYS[1], jobs[i])
    queue = ARGV[1] .. j['name']
    found = false
    for _,v in pairs(KEYS) do
      if v == queue then
        j['t'] = tonumber(ARGV[2])
        j['fails'] = nil
        j['failed_at'] = nil
        j['err'] = nil
        redis.call('lpush', queue, cjson.encode(j))
        requeuedCount = requeuedCount + 1
        found = true
        break
      end
    end
    if not found then
      j['err'] = 'unknown job when requeueing'
      j['failed_at'] = tonumber(ARGV[2])
      redis.call('zadd', KEYS[1], ARGV[2] + 5, cjson.encode(j))
    end
  end
end
return requeuedCount
`

// KEYS[1] = zset of dead jobs, eg work:dead
// KEYS[2...] = known job queues, eg ["work:jobs:create_watch", "work:jobs:send_email", ...]
// ARGV[1] = jobs prefix, eg, "work:jobs:". We'll take that and append the job name from the JSON object in order to queue up a job
// ARGV[2] = current time in epoch seconds
// ARGV[3] = max number of jobs to requeue
// Returns: number of jobs requeued
var redisLuaRequeueAllDeadCmd = `
local jobs, i, j, queue, found, requeuedCount
jobs = redis.call('zrangebyscore', KEYS[1], '-inf', ARGV[2], 'LIMIT', 0, ARGV[3])
local jobCount = #jobs
requeuedCount = 0
for i=1,jobCount do
  j = cjson.decode(jobs[i])
  redis.call('zrem', KEYS[1], jobs[i])
  queue = ARGV[1] .. j['name']
  found = false
  for _,v in pairs(KEYS) do
    if v == queue then
      j['t'] = tonumber(ARGV[2])
      j['fails'] = nil
      j['failed_at'] = nil
      j['err'] = nil
      redis.call('lpush', queue, cjson.encode(j))
      requeuedCount = requeuedCount + 1
      found = true
      break
    end
  end
  if not found then
    j['err'] = 'unknown job when requeueing'
    j['failed_at'] = tonumber(ARGV[2])
    redis.call('zadd', KEYS[1], ARGV[2] + 5, cjson.encode(j))
  end
end
return requeuedCount
`

// KEYS[1] = job queue to push onto
// KEYS[2] = Unique job's key. Test for existence and set if we push.
// ARGV[1] = job
var redisLuaEnqueueUnique = `
if redis.call('set', KEYS[2], '1', 'NX', 'EX', '86400') then
  redis.call('lpush', KEYS[1], ARGV[1])
  return 'ok'
end
return 'dup'
`

// KEYS[1] = scheduled job queue
// KEYS[2] = Unique job's key. Test for existence and set if we push.
// ARGV[1] = job
// ARGV[2] = epoch seconds for job to be run at
var redisLuaEnqueueUniqueIn = `
if redis.call('set', KEYS[2], '1', 'NX', 'EX', '86400') then
  redis.call('zadd', KEYS[1], ARGV[2], ARGV[1])
  return 'ok'
end
return 'dup'
`

// Used by the reaper to release acquired lock.
//
// KEYS[1] = reaper lock key
// ARGV[1] = reaper lock random value
var redisReleaseLockScript = redis.NewScript(1, `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
`)

// Used by the reaper to get unknown pool IDs and associated job lock_info keys.
//
// KEYS[1] = worker pools key
// KEYS[2...] = known job's lock info
// Returns: {"3a8b74d991320d6283bb4363": ["ns:jobs:job1:lock_info", "ns:jobs:job2:lock_info"]}
var redisGetUnknownPoolsScript = redis.NewScript(-1, `
local poolsKey = KEYS[1]
local unknownPools = {}

for i=2,#KEYS do
    local lockInfoKey = KEYS[i]
    local poolIDs = redis.call('hkeys', lockInfoKey)

    for j=1,#poolIDs do
        local poolID = poolIDs[j]
        local isMemberOfPools = redis.call('sismember', poolsKey, poolID) == 1

        if not isMemberOfPools then
            local pool = unknownPools[poolID]
            if pool == nil then
                pool = {}
            end

            table.insert(pool, lockInfoKey)
            unknownPools[poolID] = pool
        end
    end
end

return cjson.encode(unknownPools)
`)

// Used by the reaper to DECR dangling locks. Returns the dangling lock keys that
// have been fixed.
//
// KEYS[1] = job's lock key
// KEYS[2...] = job's lock info key
// Returns: ["ns:jobs:job1:lock", "ns:jobs:job3:lock"]
var redisRemoveDanglingLocksScript = redis.NewScript(-1, `
local danglingLocks = {}

for i=1,#KEYS,2 do
    local lockKey = KEYS[i]
    local lockInfoKey = KEYS[i+1]

    local rlocks = redis.call('get', lockKey)
    if rlocks ~= false then
        local locks = tonumber(rlocks)
        local lockInfo = redis.call('hvals', lockInfoKey)

        local totalLocks = 0
        for j=1,#lockInfo do
            totalLocks = totalLocks + tonumber(lockInfo[j])
        end

        local diff = locks - totalLocks
        if diff ~= 0 then
            table.insert(danglingLocks, lockKey)
            redis.call('decrby', lockKey, diff)
        end
    end
end

return danglingLocks
`)
