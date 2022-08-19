package datastore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/flashbots/go-boost-utils/types"
	"github.com/go-redis/redis/v9"
)

var (
	redisPrefix = "boost-relay"

	expiryBidCache   = 5 * time.Minute
	expiryBeaconNode = 5 * time.Minute

	RedisConfigFieldPubkey    = "pubkey"
	RedisStatsFieldLatestSlot = "latest-slot"
)

func PubkeyHexToLowerStr(pk types.PubkeyHex) string {
	return strings.ToLower(string(pk))
}

func connectRedis(redisURI string) (*redis.Client, error) {
	redisClient := redis.NewClient(&redis.Options{
		Addr: redisURI,
	})
	if _, err := redisClient.Ping(context.Background()).Result(); err != nil {
		// unable to connect to redis
		return nil, err
	}
	return redisClient, nil
}

type RedisCache struct {
	client *redis.Client

	prefixBidCache string

	keyKnownValidators                string
	keyValidatorRegistration          string
	keyValidatorRegistrationTimestamp string

	keyRelayConfig    string
	keyStats          string
	keyProposerDuties string
	keyBeaconNode     string
}

func NewRedisCache(redisURI, prefix string) (*RedisCache, error) {
	client, err := connectRedis(redisURI)
	if err != nil {
		return nil, err
	}

	return &RedisCache{
		client: client,

		prefixBidCache: fmt.Sprintf("%s/%s:bid-cache", redisPrefix, prefix),

		keyKnownValidators:                fmt.Sprintf("%s/%s:known-validators", redisPrefix, prefix),
		keyValidatorRegistration:          fmt.Sprintf("%s/%s:validators-registration-timestamp", redisPrefix, prefix),
		keyValidatorRegistrationTimestamp: fmt.Sprintf("%s/%s:validators-registration", redisPrefix, prefix),
		keyRelayConfig:                    fmt.Sprintf("%s/%s:relay-config", redisPrefix, prefix),
		keyBeaconNode:                     fmt.Sprintf("%s/%s:beacon-node", redisPrefix, prefix),

		keyStats:          fmt.Sprintf("%s/%s:stats", redisPrefix, prefix),
		keyProposerDuties: fmt.Sprintf("%s/%s:proposer-duties", redisPrefix, prefix),
	}, nil
}

func (r *RedisCache) keyBidCache(slot uint64, parentHash, proposerPubkey string) string {
	return fmt.Sprintf("%s:%d_%s_%s", r.prefixBidCache, slot, parentHash, proposerPubkey)
}

func (r *RedisCache) GetObj(key string, obj any) (err error) {
	value, err := r.client.Get(context.Background(), key).Result()
	if err != nil {
		return err
	}

	return json.Unmarshal([]byte(value), &obj)
}

func (r *RedisCache) SetObj(key string, value any, expiration time.Duration) (err error) {
	marshalledValue, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return r.client.Set(context.Background(), key, marshalledValue, expiration).Err()
}

func (r *RedisCache) GetKnownValidators() (map[types.PubkeyHex]uint64, error) {
	validators := make(map[types.PubkeyHex]uint64)
	entries, err := r.client.HGetAll(context.Background(), r.keyKnownValidators).Result()
	if err != nil {
		return nil, err
	}
	for pubkey, proposerIndexStr := range entries {
		proposerIndex, err := strconv.ParseUint(proposerIndexStr, 10, 64)
		// TODO: log on error
		if err == nil {
			validators[types.PubkeyHex(pubkey)] = proposerIndex
		}
	}
	return validators, nil
}

func (r *RedisCache) SetKnownValidator(pubkeyHex types.PubkeyHex, proposerIndex uint64) error {
	return r.client.HSet(context.Background(), r.keyKnownValidators, PubkeyHexToLowerStr(pubkeyHex), proposerIndex).Err()
}

func (r *RedisCache) GetValidatorRegistration(proposerPubkey types.PubkeyHex) (*types.SignedValidatorRegistration, error) {
	registration := new(types.SignedValidatorRegistration)
	value, err := r.client.HGet(context.Background(), r.keyValidatorRegistration, strings.ToLower(proposerPubkey.String())).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	err = json.Unmarshal([]byte(value), registration)
	return registration, err
}

func (r *RedisCache) GetValidatorRegistrationTimestamp(proposerPubkey types.PubkeyHex) (uint64, error) {
	timestamp, err := r.client.HGet(context.Background(), r.keyValidatorRegistrationTimestamp, strings.ToLower(proposerPubkey.String())).Uint64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return timestamp, err
}

func (r *RedisCache) SetValidatorRegistration(entry types.SignedValidatorRegistration) error {
	err := r.client.HSet(context.Background(), r.keyValidatorRegistrationTimestamp, strings.ToLower(entry.Message.Pubkey.PubkeyHex().String()), entry.Message.Timestamp).Err()
	if err != nil {
		return err
	}

	marshalledValue, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	err = r.client.HSet(context.Background(), r.keyValidatorRegistration, strings.ToLower(entry.Message.Pubkey.PubkeyHex().String()), marshalledValue).Err()
	return err
}

func (r *RedisCache) SetValidatorRegistrations(entries []types.SignedValidatorRegistration) error {
	for _, entry := range entries {
		err := r.SetValidatorRegistration(entry)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *RedisCache) NumRegisteredValidators() (int64, error) {
	return r.client.HLen(context.Background(), r.keyValidatorRegistrationTimestamp).Result()
}

func (r *RedisCache) SetStats(field string, value any) (err error) {
	return r.client.HSet(context.Background(), r.keyStats, field, value).Err()
}

func (r *RedisCache) GetStats(field string) (value string, err error) {
	return r.client.HGet(context.Background(), r.keyStats, field).Result()
}

func (r *RedisCache) SetProposerDuties(proposerDuties []types.BuilderGetValidatorsResponseEntry) (err error) {
	return r.SetObj(r.keyProposerDuties, proposerDuties, 0)
}

func (r *RedisCache) GetProposerDuties() (proposerDuties []types.BuilderGetValidatorsResponseEntry, err error) {
	proposerDuties = make([]types.BuilderGetValidatorsResponseEntry, 0)
	err = r.GetObj(r.keyProposerDuties, &proposerDuties)
	if errors.Is(err, redis.Nil) {
		return proposerDuties, nil
	}
	return proposerDuties, err
}

func (r *RedisCache) SetRelayConfig(field, value string) (err error) {
	return r.client.HSet(context.Background(), r.keyRelayConfig, field, value).Err()
}

func (r *RedisCache) GetRelayConfig(field string) (string, error) {
	res, err := r.client.HGet(context.Background(), r.keyRelayConfig, field).Result()
	if errors.Is(err, redis.Nil) {
		return res, nil
	}
	return res, err
}

func (r *RedisCache) SaveBid(slot uint64, parentHash, proposerPubkey string, headerResp *types.GetHeaderResponse) (err error) {
	key := r.keyBidCache(slot, parentHash, proposerPubkey)
	return r.SetObj(key, headerResp, expiryBidCache)
}

// GetBid retrieves a bid from Redis. If not existing, returns nil for both bid and error
func (r *RedisCache) GetBid(slot uint64, parentHash, proposerPubkey string) (bid *types.GetHeaderResponse, err error) {
	key := r.keyBidCache(slot, parentHash, proposerPubkey)
	bid = new(types.GetHeaderResponse)
	err = r.GetObj(key, bid)
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return bid, err
}

// SetBeaconNodeIndex sets the last beacon node that sent a successful response
func (r *RedisCache) SetBeaconNodeIndex(beaconNodeIndex int) (err error) {
	return r.client.Set(context.Background(), r.keyBeaconNode, beaconNodeIndex, expiryBeaconNode).Err()
}

// GetBeaconNodeIndex retrieves the last beacon node that sent a successful response
func (r *RedisCache) GetBeaconNodeIndex() (beaconNodeIndex int, err error) {
	index, err := r.client.Get(context.Background(), r.keyBeaconNode).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return index, err
}
