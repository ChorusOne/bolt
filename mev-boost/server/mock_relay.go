package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	builderApi "github.com/attestantio/go-builder-client/api"
	builderApiCapella "github.com/attestantio/go-builder-client/api/capella"
	builderApiDeneb "github.com/attestantio/go-builder-client/api/deneb"
	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	builderSpec "github.com/attestantio/go-builder-client/spec"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	utilbellatrix "github.com/attestantio/go-eth2-client/util/bellatrix"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flashbots/go-boost-utils/bls"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/gorilla/mux"
	"github.com/holiman/uint256"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

const (
	mockRelaySecretKeyHex = "0x4e343a647c5a5c44d76c2c58b63f02cdf3a9a0ec40f102ebc26363b4b1b95033"
)

var (
	skBytes, _            = hexutil.Decode(mockRelaySecretKeyHex)
	mockRelaySecretKey, _ = bls.SecretKeyFromBytes(skBytes)
	mockRelayPublicKey, _ = bls.PublicKeyFromSecretKey(mockRelaySecretKey)
)

// mockRelay is used to fake a relay's behavior.
// You can override each of its handler by setting the instance's HandlerOverride_METHOD_TO_OVERRIDE to your own
// handler.
type mockRelay struct {
	// Used to panic if impossible error happens
	t *testing.T

	// KeyPair used to sign messages
	secretKey  *bls.SecretKey
	publicKey  *bls.PublicKey
	RelayEntry RelayEntry

	// Used to count each Request made to the relay, either if it fails or not, for each method
	mu           sync.Mutex
	requestCount map[string]int

	// Overriders
	handlerOverrideRegisterValidator   func(w http.ResponseWriter, req *http.Request)
	handlerOverrideSubmitConstraint    func(w http.ResponseWriter, req *http.Request)
	handlerOverrideGetHeader           func(w http.ResponseWriter, req *http.Request)
	handlerOverrideGetHeaderWithProofs func(w http.ResponseWriter, req *http.Request)
	handlerOverrideGetPayload          func(w http.ResponseWriter, req *http.Request)

	// Default responses placeholders, used if overrider does not exist
	GetHeaderResponse           *builderSpec.VersionedSignedBuilderBid
	GetHeaderWithProofsResponse *BidWithInclusionProofs
	GetPayloadResponse          *builderApi.VersionedSubmitBlindedBlockResponse

	// Server section
	Server        *httptest.Server
	ResponseDelay time.Duration
}

// newMockRelay creates a mocked relay which implements the backend.BoostBackend interface
// A secret key must be provided to sign default and custom response messages
func newMockRelay(t *testing.T) *mockRelay {
	t.Helper()
	relay := &mockRelay{t: t, secretKey: mockRelaySecretKey, publicKey: mockRelayPublicKey, requestCount: make(map[string]int)}

	// Initialize server
	relay.Server = httptest.NewServer(relay.getRouter())

	// Create the RelayEntry with correct pubkey
	url, err := url.Parse(relay.Server.URL)
	require.NoError(t, err)
	urlWithKey := fmt.Sprintf("%s://%s@%s", url.Scheme, hexutil.Encode(bls.PublicKeyToBytes(mockRelayPublicKey)), url.Host)
	relay.RelayEntry, err = NewRelayEntry(urlWithKey)
	require.NoError(t, err)
	return relay
}

// newTestMiddleware creates a middleware which increases the Request counter and creates a fake delay for the response
func (m *mockRelay) newTestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// Request counter
			m.mu.Lock()
			url := r.URL.EscapedPath()
			m.requestCount[url]++
			m.mu.Unlock()

			// Artificial Delay
			if m.ResponseDelay > 0 {
				time.Sleep(m.ResponseDelay)
			}

			next.ServeHTTP(w, r)
		},
	)
}

// getRouter registers all methods from the backend, apply the test middleware and return the configured router
func (m *mockRelay) getRouter() http.Handler {
	// Create router.
	r := mux.NewRouter()

	// Register handlers
	r.HandleFunc("/", m.handleRoot).Methods(http.MethodGet)
	r.HandleFunc(pathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(pathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)
	r.HandleFunc(pathGetHeader, m.handleGetHeader).Methods(http.MethodGet)
	r.HandleFunc(pathGetHeaderWithProofs, m.handleGetHeaderWithProofs).Methods(http.MethodGet)
	r.HandleFunc(pathSubmitConstraint, m.handleSubmitConstraint).Methods(http.MethodPost)
	r.HandleFunc(pathGetPayload, m.handleGetPayload).Methods(http.MethodPost)

	return m.newTestMiddleware(r)
}

// GetRequestCount returns the number of Request made to a specific URL
func (m *mockRelay) GetRequestCount(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requestCount[path]
}

// By default, handleRoot returns the relay's status
func (m *mockRelay) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{}`)
}

// By default, handleStatus returns the relay's status as http.StatusOK
func (m *mockRelay) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{}`)
}

// By default, handleRegisterValidator returns a default builderApiV1.SignedValidatorRegistration
func (m *mockRelay) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlerOverrideRegisterValidator != nil {
		m.handlerOverrideRegisterValidator(w, req)
		return
	}
	m.defaultHandleRegisterValidator(w, req)
}

// defaultHandleRegisterValidator returns the default handler for handleRegisterValidator
func (m *mockRelay) defaultHandleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	payload := []builderApiV1.SignedValidatorRegistration{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}

func (m *mockRelay) handleSubmitConstraint(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlerOverrideSubmitConstraint != nil {
		m.handlerOverrideSubmitConstraint(w, req)
		return
	}
	m.defaultHandleSubmitConstraint(w, req)
}

func (m *mockRelay) defaultHandleSubmitConstraint(w http.ResponseWriter, req *http.Request) {
	payload := BatchedSignedConstraints{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}

func (m *mockRelay) MakeGetHeaderWithConstraintsResponse(value uint64, blockHash, parentHash, publicKey string, version spec.DataVersion, constraints []struct {
	tx   Transaction
	hash phase0.Hash32
},
) *BidWithInclusionProofs {
	transactions := new(utilbellatrix.ExecutionPayloadTransactions)

	for _, con := range constraints {
		transactions.Transactions = append(transactions.Transactions, bellatrix.Transaction(con.tx))
	}

	rootNode, err := transactions.GetTree()
	if err != nil {
		panic(err)
	}

	// BOLT: Set the value of nodes. This is MANDATORY for the proof calculation
	// to output the leaf correctly. This is also never documented in fastssz. -__-
	// Also calculates the transactions_root
	txsRoot := rootNode.Hash()

	bidWithProofs := m.MakeGetHeaderWithProofsResponseWithTxsRoot(value, blockHash, parentHash, publicKey, version, phase0.Root(txsRoot))

	// Calculate the inclusion proof
	inclusionProof, err := CalculateMerkleMultiProofs(rootNode, constraints)
	if err != nil {
		logrus.WithError(err).Error("failed to calculate inclusion proof")
		return nil
	}

	bidWithProofs.Proofs = inclusionProof

	return bidWithProofs
}

// MakeGetHeaderResponse is used to create the default or can be used to create a custom response to the getHeader
// method
func (m *mockRelay) MakeGetHeaderResponse(value uint64, blockHash, parentHash, publicKey string, version spec.DataVersion) *builderSpec.VersionedSignedBuilderBid {
	switch version {
	case spec.DataVersionCapella:
		// Fill the payload with custom values.
		message := &builderApiCapella.BuilderBid{
			Header: &capella.ExecutionPayloadHeader{
				BlockHash:       _HexToHash(blockHash),
				ParentHash:      _HexToHash(parentHash),
				WithdrawalsRoot: phase0.Root{},
			},
			Value:  uint256.NewInt(value),
			Pubkey: _HexToPubkey(publicKey),
		}

		// Sign the message.
		signature, err := ssz.SignMessage(message, ssz.DomainBuilder, m.secretKey)
		require.NoError(m.t, err)

		return &builderSpec.VersionedSignedBuilderBid{
			Version: spec.DataVersionCapella,
			Capella: &builderApiCapella.SignedBuilderBid{
				Message:   message,
				Signature: signature,
			},
		}
	case spec.DataVersionDeneb:

		message := &builderApiDeneb.BuilderBid{
			Header: &deneb.ExecutionPayloadHeader{
				BlockHash:       _HexToHash(blockHash),
				ParentHash:      _HexToHash(parentHash),
				WithdrawalsRoot: phase0.Root{},
				BaseFeePerGas:   uint256.NewInt(0),
			},
			BlobKZGCommitments: make([]deneb.KZGCommitment, 0),
			Value:              uint256.NewInt(value),
			Pubkey:             _HexToPubkey(publicKey),
		}

		// Sign the message.
		signature, err := ssz.SignMessage(message, ssz.DomainBuilder, m.secretKey)
		require.NoError(m.t, err)

		return &builderSpec.VersionedSignedBuilderBid{
			Version: spec.DataVersionDeneb,
			Deneb: &builderApiDeneb.SignedBuilderBid{
				Message:   message,
				Signature: signature,
			},
		}
	case spec.DataVersionUnknown, spec.DataVersionPhase0, spec.DataVersionAltair, spec.DataVersionBellatrix:
		return nil
	}
	return nil
}

// MakeGetHeaderWithProofsResponseWithTxsRoot is used to create the default or can be used to create a custom response to the getHeaderWithProofs
// method
func (m *mockRelay) MakeGetHeaderWithProofsResponseWithTxsRoot(value uint64, blockHash, parentHash, publicKey string, version spec.DataVersion, txsRoot phase0.Root) *BidWithInclusionProofs {
	switch version {
	case spec.DataVersionCapella:
		// Fill the payload with custom values.
		message := &builderApiCapella.BuilderBid{
			Header: &capella.ExecutionPayloadHeader{
				BlockHash:        _HexToHash(blockHash),
				ParentHash:       _HexToHash(parentHash),
				WithdrawalsRoot:  phase0.Root{},
				TransactionsRoot: txsRoot,
			},
			Value:  uint256.NewInt(value),
			Pubkey: _HexToPubkey(publicKey),
		}

		// Sign the message.
		signature, err := ssz.SignMessage(message, ssz.DomainBuilder, m.secretKey)
		require.NoError(m.t, err)

		return &BidWithInclusionProofs{
			Bid: &builderSpec.VersionedSignedBuilderBid{
				Version: spec.DataVersionCapella,
				Capella: &builderApiCapella.SignedBuilderBid{
					Message:   message,
					Signature: signature,
				},
			},
		}
	case spec.DataVersionDeneb:

		message := &builderApiDeneb.BuilderBid{
			Header: &deneb.ExecutionPayloadHeader{
				BlockHash:        _HexToHash(blockHash),
				ParentHash:       _HexToHash(parentHash),
				WithdrawalsRoot:  phase0.Root{},
				BaseFeePerGas:    uint256.NewInt(0),
				TransactionsRoot: txsRoot,
			},
			BlobKZGCommitments: make([]deneb.KZGCommitment, 0),
			Value:              uint256.NewInt(value),
			Pubkey:             _HexToPubkey(publicKey),
		}

		// Sign the message.
		signature, err := ssz.SignMessage(message, ssz.DomainBuilder, m.secretKey)
		require.NoError(m.t, err)

		return &BidWithInclusionProofs{
			Bid: &builderSpec.VersionedSignedBuilderBid{
				Version: spec.DataVersionDeneb,
				Deneb: &builderApiDeneb.SignedBuilderBid{
					Message:   message,
					Signature: signature,
				},
			},
		}
	case spec.DataVersionUnknown, spec.DataVersionPhase0, spec.DataVersionAltair, spec.DataVersionBellatrix:
		return nil
	}
	return nil
}

// handleGetHeader handles incoming requests to server.pathGetHeader
func (m *mockRelay) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Try to override default behavior is custom handler is specified.
	if m.handlerOverrideGetHeader != nil {
		m.handlerOverrideGetHeader(w, req)
		return
	}
	m.defaultHandleGetHeader(w)
}

// defaultHandleGetHeader returns the default handler for handleGetHeader
func (m *mockRelay) defaultHandleGetHeader(w http.ResponseWriter) {
	// By default, everything will be ok.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Build the default response.
	response := m.MakeGetHeaderResponse(
		12345,
		"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
		"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
		"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
		spec.DataVersionCapella,
	)

	if m.GetHeaderResponse != nil {
		response = m.GetHeaderResponse
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// handleGetHeaderWithProofs handles incoming requests to server.pathGetHeader
func (m *mockRelay) handleGetHeaderWithProofs(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Try to override default behavior is custom handler is specified.
	if m.handlerOverrideGetHeader != nil {
		m.handlerOverrideGetHeaderWithProofs(w, req)
		return
	}
	m.defaultHandleGetHeaderWithProofs(w)
}

// defaultHandleGetHeaderWithProofs returns the default handler for handleGetHeaderWithProofs
func (m *mockRelay) defaultHandleGetHeaderWithProofs(w http.ResponseWriter) {
	// By default, everything will be ok.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Build the default response.
	response := m.MakeGetHeaderWithConstraintsResponse(
		12345,
		"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
		"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
		"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
		spec.DataVersionCapella,
		nil,
	)

	if m.GetHeaderWithProofsResponse != nil {
		response = m.GetHeaderWithProofsResponse
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// MakeGetPayloadResponse is used to create the default or can be used to create a custom response to the getPayload
// method
func (m *mockRelay) MakeGetPayloadResponse(parentHash, blockHash, feeRecipient string, blockNumber uint64, version spec.DataVersion) *builderApi.VersionedSubmitBlindedBlockResponse {
	return &builderApi.VersionedSubmitBlindedBlockResponse{
		Version: version,
		Capella: &capella.ExecutionPayload{
			ParentHash:   _HexToHash(parentHash),
			BlockHash:    _HexToHash(blockHash),
			BlockNumber:  blockNumber,
			FeeRecipient: _HexToAddress(feeRecipient),
			Withdrawals:  make([]*capella.Withdrawal, 0),
		},
	}
}

// handleGetPayload handles incoming requests to server.pathGetPayload
func (m *mockRelay) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Try to override default behavior is custom handler is specified.
	if m.handlerOverrideGetPayload != nil {
		m.handlerOverrideGetPayload(w, req)
		return
	}
	m.defaultHandleGetPayload(w)
}

// defaultHandleGetPayload returns the default handler for handleGetPayload
func (m *mockRelay) defaultHandleGetPayload(w http.ResponseWriter) {
	// By default, everything will be ok.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Build the default response.
	response := m.MakeGetPayloadResponse(
		"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
		"0x534809bd2b6832edff8d8ce4cb0e50068804fd1ef432c8362ad708a74fdc0e46",
		"0xdb65fEd33dc262Fe09D9a2Ba8F80b329BA25f941",
		12345,
		spec.DataVersionCapella,
	)

	if m.GetPayloadResponse != nil {
		response = m.GetPayloadResponse
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (m *mockRelay) overrideHandleRegisterValidator(method func(w http.ResponseWriter, req *http.Request)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.handlerOverrideRegisterValidator = method
}
