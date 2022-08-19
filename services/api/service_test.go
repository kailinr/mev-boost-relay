package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/flashbots/boost-relay/beaconclient"
	"github.com/flashbots/boost-relay/common"
	"github.com/flashbots/boost-relay/database"
	"github.com/flashbots/boost-relay/datastore"
	"github.com/flashbots/go-boost-utils/bls"
	"github.com/flashbots/go-boost-utils/types"
	"github.com/stretchr/testify/require"
)

var (
	genesisForkVersionHex = "0x00000000"
	builderSigningDomain  = types.Domain([32]byte{0, 0, 0, 1, 245, 165, 253, 66, 209, 106, 32, 48, 39, 152, 239, 110, 211, 9, 151, 155, 67, 0, 61, 35, 32, 217, 240, 232, 234, 152, 49, 169})
	errTest               = errors.New("test error")
)

type testBackend struct {
	t             require.TestingT
	relay         *RelayAPI
	beaconClients []*beaconclient.MockBeaconClient
	datastore     *datastore.Datastore
	redis         *datastore.RedisCache
}

func newTestBackend(t require.TestingT, numBeaconNodes int) *testBackend {
	mockBeaconClients := make([]*beaconclient.MockBeaconClient, numBeaconNodes)
	mockBeaconClientsInterface := make([]beaconclient.BeaconNodeClient, numBeaconNodes)
	for i := 0; i < numBeaconNodes; i++ {
		mockBeaconClients[i] = beaconclient.NewMockBeaconClient()
		mockBeaconClientsInterface[i] = mockBeaconClients[i]
	}

	redisClient, err := miniredis.Run()
	require.NoError(t, err)

	redisCache, err := datastore.NewRedisCache(redisClient.Addr(), "")
	require.NoError(t, err)

	db := database.MockDB{}

	ds, err := datastore.NewDatastore(common.TestLog, redisCache, db)
	require.NoError(t, err)

	sk, _, err := bls.GenerateNewKeypair()
	require.NoError(t, err)

	opts := RelayAPIOpts{
		Log:           common.TestLog,
		ListenAddr:    "localhost:12345",
		BeaconClients: mockBeaconClientsInterface,
		Datastore:     ds,
		Redis:         redisCache,
		DB:            db,
		EthNetDetails: common.EthNetworkDetails{
			Name:                     "test",
			GenesisForkVersionHex:    genesisForkVersionHex,
			GenesisValidatorsRootHex: "",
			BellatrixForkVersionHex:  "0x00000000",
			DomainBuilder:            types.Domain{},
			DomainBeaconProposer:     types.Domain{},
		},
		SecretKey: sk,
	}

	relay, err := NewRelayAPI(opts)
	require.NoError(t, err)

	backend := testBackend{
		t:             t,
		relay:         relay,
		beaconClients: mockBeaconClients,
		datastore:     ds,
		redis:         redisCache,
	}
	return &backend
}

func (be *testBackend) request(method, path string, payload any) *httptest.ResponseRecorder {
	var req *http.Request
	var err error

	if payload == nil {
		req, err = http.NewRequest(method, path, bytes.NewReader(nil))
	} else {
		payloadBytes, err2 := json.Marshal(payload)
		require.NoError(be.t, err2)
		req, err = http.NewRequest(method, path, bytes.NewReader(payloadBytes))
	}

	require.NoError(be.t, err)
	rr := httptest.NewRecorder()
	be.relay.getRouter().ServeHTTP(rr, req)
	return rr
}

func generateSignedValidatorRegistration(sk *bls.SecretKey, feeRecipient types.Address, timestamp uint64) (*types.SignedValidatorRegistration, error) {
	var err error
	if sk == nil {
		sk, _, err = bls.GenerateNewKeypair()
		if err != nil {
			return nil, err
		}
	}

	blsPubKey := bls.PublicKeyFromSecretKey(sk)

	var pubKey types.PublicKey
	err = pubKey.FromSlice(blsPubKey.Compress())
	if err != nil {
		return nil, err
	}
	msg := &types.RegisterValidatorRequestMessage{
		FeeRecipient: feeRecipient,
		Timestamp:    timestamp,
		Pubkey:       pubKey,
		GasLimit:     278234191203,
	}

	sig, err := types.SignMessage(msg, builderSigningDomain, sk)
	if err != nil {
		return nil, err
	}

	return &types.SignedValidatorRegistration{
		Message:   msg,
		Signature: sig,
	}, nil
}

func TestWebserver(t *testing.T) {
	t.Run("errors when webserver is already existing", func(t *testing.T) {
		backend := newTestBackend(t, 1)
		backend.relay.srvStarted.Store(true)
		err := backend.relay.StartServer()
		require.Error(t, err)
	})
}

func TestGetSyncStatus(t *testing.T) {
	t.Run("returns status of the beacon node first to respond and is syncing", func(t *testing.T) {
		syncStatuses := []*beaconclient.SyncStatusPayloadData{
			{
				HeadSlot:  3,
				IsSyncing: true,
			},
			{
				HeadSlot:  1,
				IsSyncing: false,
			},
			{
				HeadSlot:  2,
				IsSyncing: false,
			},
		}

		backend := newTestBackend(t, 3)
		for i := 0; i < len(backend.beaconClients); i++ {
			backend.beaconClients[i].MockSyncStatus = syncStatuses[i]
			backend.beaconClients[i].ResponseDelay = 10 * time.Millisecond * time.Duration(i)
		}

		status, err := backend.relay.getBestSyncStatus()
		require.NoError(t, err)
		require.Equal(t, syncStatuses[1], status)
	})

	t.Run("returns status if at least one beacon node does not return error and is synced", func(t *testing.T) {
		backend := newTestBackend(t, 2)
		backend.beaconClients[0].MockSyncStatusErr = errTest
		status, err := backend.relay.getBestSyncStatus()
		require.NoError(t, err)
		require.NotNil(t, status)
	})

	t.Run("returns error if all beacon nodes return error or syncing", func(t *testing.T) {
		backend := newTestBackend(t, 2)
		backend.beaconClients[0].MockSyncStatusErr = errTest
		backend.beaconClients[1].MockSyncStatus = &beaconclient.SyncStatusPayloadData{
			HeadSlot:  1,
			IsSyncing: true,
		}
		status, err := backend.relay.getBestSyncStatus()
		require.Equal(t, ErrBeaconNodeSyncing, err)
		require.Nil(t, status)
	})
}

func TestWebserverRootHandler(t *testing.T) {
	backend := newTestBackend(t, 1)
	rr := backend.request(http.MethodGet, "/", nil)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestStatus(t *testing.T) {
	backend := newTestBackend(t, 1)
	path := "/eth/v1/builder/status"
	rr := backend.request(http.MethodGet, path, common.ValidPayloadRegisterValidator)
	require.Equal(t, http.StatusOK, rr.Code)
}

func TestRegisterValidator(t *testing.T) {
	path := "/eth/v1/builder/validators"

	t.Run("Normal function", func(t *testing.T) {
		t.Skip() // has an error at verifying the sig

		backend := newTestBackend(t, 1)
		err := backend.relay.startValidatorRegistrationWorkers()
		require.NoError(t, err)
		pubkeyHex := common.ValidPayloadRegisterValidator.Message.Pubkey.PubkeyHex()
		index := uint64(17)
		err = backend.redis.SetKnownValidator(pubkeyHex, index)
		require.NoError(t, err)

		// Update datastore
		_, err = backend.datastore.RefreshKnownValidators()
		require.NoError(t, err)
		require.True(t, backend.datastore.IsKnownValidator(pubkeyHex))
		pkH, ok := backend.datastore.GetKnownValidatorPubkeyByIndex(index)
		require.True(t, ok)
		require.Equal(t, pubkeyHex, pkH)

		rr := backend.request(http.MethodPost, path, []types.SignedValidatorRegistration{common.ValidPayloadRegisterValidator})
		require.Equal(t, http.StatusOK, rr.Code)
		time.Sleep(20 * time.Millisecond) // registrations are processed asynchronously

		req, err := backend.datastore.GetValidatorRegistration(pubkeyHex)
		require.NoError(t, err)
		require.NotNil(t, req)
		require.Equal(t, pubkeyHex, req.Message.Pubkey.PubkeyHex())
	})

	t.Run("not a known validator", func(t *testing.T) {
		backend := newTestBackend(t, 1)

		rr := backend.request(http.MethodPost, path, []types.SignedValidatorRegistration{common.ValidPayloadRegisterValidator})
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("Reject registration for >10sec into the future", func(t *testing.T) {
		backend := newTestBackend(t, 1)

		// Allow +10 sec
		td := uint64(time.Now().Unix())
		payload, err := generateSignedValidatorRegistration(nil, types.Address{1}, td+10)
		require.NoError(t, err)
		err = backend.redis.SetKnownValidator(payload.Message.Pubkey.PubkeyHex(), 1)
		require.NoError(t, err)
		_, err = backend.datastore.RefreshKnownValidators()
		require.NoError(t, err)

		rr := backend.request(http.MethodPost, path, []types.SignedValidatorRegistration{*payload})
		require.Equal(t, http.StatusOK, rr.Code)

		// Disallow +11 sec
		td = uint64(time.Now().Unix())
		payload, err = generateSignedValidatorRegistration(nil, types.Address{1}, td+11)
		require.NoError(t, err)
		err = backend.redis.SetKnownValidator(payload.Message.Pubkey.PubkeyHex(), 1)
		require.NoError(t, err)
		_, err = backend.datastore.RefreshKnownValidators()
		require.NoError(t, err)

		rr = backend.request(http.MethodPost, path, []types.SignedValidatorRegistration{*payload})
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "timestamp too far in the future")
	})
}

func TestBuilderApiGetValidators(t *testing.T) {
	path := "/relay/v1/builder/validators"

	backend := newTestBackend(t, 1)
	backend.relay.proposerDutiesResponse = []types.BuilderGetValidatorsResponseEntry{
		{
			Slot:  1,
			Entry: &common.ValidPayloadRegisterValidator,
		},
	}

	rr := backend.request(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	resp := []types.BuilderGetValidatorsResponseEntry{}
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, 1, len(resp))
	require.Equal(t, uint64(1), resp[0].Slot)
	require.Equal(t, common.ValidPayloadRegisterValidator, *resp[0].Entry)
}

func TestDataApiGetDataProposerPayloadDelivered(t *testing.T) {
	path := "/relay/v1/data/bidtraces/proposer_payload_delivered"

	t.Run("Accept valid block_hash", func(t *testing.T) {
		backend := newTestBackend(t, 1)

		validBlockHash := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		rr := backend.request(http.MethodGet, path+"?block_hash="+validBlockHash, nil)
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Reject invalid block_hash", func(t *testing.T) {
		backend := newTestBackend(t, 1)

		invalidBlockHashes := []string{
			// One character too long.
			"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab",
			// One character too short.
			"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			// Missing the 0x prefix.
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			// Has an invalid hex character ('z' at the end).
			"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaz",
		}

		for _, invalidBlockHash := range invalidBlockHashes {
			rr := backend.request(http.MethodGet, path+"?block_hash="+invalidBlockHash, nil)
			t.Log(invalidBlockHash)
			require.Equal(t, http.StatusBadRequest, rr.Code)
			require.Contains(t, rr.Body.String(), "invalid block_hash argument")
		}
	})
}
