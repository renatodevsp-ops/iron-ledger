package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/usecase"
)

// operationRequest is decoded field by field rather than into a struct, because
// the contract sets additionalProperties: false and every rejection must name
// the offending field. A struct with DisallowUnknownFields cannot tell us which
// field was unknown, and silently ignoring it would let a caller believe a typo
// like "ammountMinor" was applied.
type operationRequest struct {
	Type          domain.OperationType
	AmountMinor   int64
	Currency      domain.Currency
	TransactionID string
	BetID         string
	OperationID   string
	ReferenceID   string
}

// knownOperationFields is the exact allowlist from the OperationRequest schema.
var knownOperationFields = map[string]struct{}{
	"type": {}, "amountMinor": {}, "currency": {}, "transactionId": {},
	"betId": {}, "operationId": {}, "referenceId": {},
}

// maxReferenceLength is the contract's bound on transactionId and referenceId.
const maxReferenceLength = 128

func parseOperationRequest(body []byte) (operationRequest, *domain.Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return operationRequest{}, domain.NewError(domain.ReasonInvalidOperationState,
			"request body must be a JSON object")
	}

	for name := range raw {
		if _, ok := knownOperationFields[name]; !ok {
			return operationRequest{}, domain.NewError(domain.ReasonUnknownField,
				"unknown field %q", name).WithField(name)
		}
	}

	var req operationRequest
	if err := decodeString(raw, "type", &req.Type); err != nil {
		return operationRequest{}, err
	}
	if err := decodeString(raw, "currency", &req.Currency); err != nil {
		return operationRequest{}, err
	}
	if err := decodeString(raw, "transactionId", &req.TransactionID); err != nil {
		return operationRequest{}, err
	}
	if err := decodeString(raw, "betId", &req.BetID); err != nil {
		return operationRequest{}, err
	}
	if err := decodeString(raw, "operationId", &req.OperationID); err != nil {
		return operationRequest{}, err
	}
	if err := decodeString(raw, "referenceId", &req.ReferenceID); err != nil {
		return operationRequest{}, err
	}

	// amountMinor is decoded through json.Number so "3000.5", "3e3" and "3000.0"
	// are all refused. Money is an integer count of minor units; accepting a
	// float here and rounding it would be the one rounding step the whole design
	// exists to avoid.
	rawAmount, ok := raw["amountMinor"]
	if !ok {
		return operationRequest{}, domain.NewError(domain.ReasonMissingField,
			"amountMinor is required").WithField("amountMinor")
	}
	amount, err := decodeMinorUnits(rawAmount)
	if err != nil {
		return operationRequest{}, domain.NewError(domain.ReasonInvalidAmount,
			"amountMinor must be a positive integer in the currency minor unit; "+
				"precision finer than the minor unit is not accepted").WithField("amountMinor")
	}
	req.AmountMinor = amount

	if req.Type == "" {
		return operationRequest{}, domain.NewError(domain.ReasonMissingField,
			"type is required").WithField("type")
	}
	if !domain.ValidOperationType(req.Type) {
		return operationRequest{}, domain.NewError(domain.ReasonUnknownOperationType,
			"unknown operation type %q", req.Type).WithField("type")
	}
	if req.Currency == "" {
		return operationRequest{}, domain.NewError(domain.ReasonMissingField,
			"currency is required").WithField("currency")
	}
	if !domain.ValidCurrency(req.Currency) {
		return operationRequest{}, domain.NewError(domain.ReasonInvalidCurrency,
			"unsupported currency %q; supported currencies are BRL and USD", req.Currency).
			WithField("currency")
	}
	if req.TransactionID == "" {
		return operationRequest{}, domain.NewError(domain.ReasonMissingField,
			"transactionId is required").WithField("transactionId")
	}
	if len(req.TransactionID) > maxReferenceLength {
		return operationRequest{}, domain.NewError(domain.ReasonInvalidOperationState,
			"transactionId must be at most %d characters", maxReferenceLength).
			WithField("transactionId")
	}
	if len(req.ReferenceID) > maxReferenceLength {
		return operationRequest{}, domain.NewError(domain.ReasonInvalidOperationState,
			"referenceId must be at most %d characters", maxReferenceLength).
			WithField("referenceId")
	}
	return req, nil
}

func decodeString(raw map[string]json.RawMessage, field string, dst any) *domain.Error {
	value, ok := raw[field]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(value, dst); err != nil {
		return domain.NewError(domain.ReasonInvalidOperationState,
			"%s has the wrong JSON type", field).WithField(field)
	}
	return nil
}

// decodeMinorUnits accepts only an exact integer literal. A string "3000" is
// refused too: the contract declares a JSON number, and accepting both would
// make the wire format ambiguous.
func decodeMinorUnits(raw json.RawMessage) (int64, error) {
	var num json.Number
	if err := json.Unmarshal(raw, &num); err != nil {
		return 0, domain.NewError(domain.ReasonInvalidAmount, "amountMinor must be a number")
	}
	text := num.String()
	if strings.ContainsAny(text, ".eE") {
		return 0, domain.NewError(domain.ReasonInvalidAmount,
			"amountMinor must be an integer, got %s", text)
	}
	value, err := num.Int64()
	if err != nil {
		return 0, domain.NewError(domain.ReasonInvalidAmount,
			"amountMinor is out of the int64 range")
	}
	if value <= 0 {
		return 0, domain.NewError(domain.ReasonInvalidAmount, "amountMinor must be greater than zero")
	}
	return value, nil
}

// applyOperation implements POST /v1/wallets/{walletId}/operations.
func (s *Server) applyOperation(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, domain.ErrUnauthorized)
		return
	}
	walletID := r.PathValue("walletId")
	if !isUUID(walletID) {
		writeError(w, domain.NewError(domain.ReasonWalletNotFound, "no such wallet").
			WithField("walletId"))
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	// The key is validated before the body: a request without one must be refused
	// outright, never processed on a best-effort basis.
	if err := usecase.ValidateIdempotencyKey(key); err != nil {
		writeError(w, err)
		return
	}

	body, bodyErr := readBody(w, r)
	if bodyErr != nil {
		writeError(w, bodyErr)
		return
	}
	req, reqErr := parseOperationRequest(body)
	if reqErr != nil {
		writeError(w, reqErr)
		return
	}

	out, err := s.svc.Apply(r.Context(), usecase.ApplyInput{
		WalletID:       walletID,
		TenantID:       principal.TenantID,
		IdempotencyKey: key,
		Operation: domain.Operation{
			Type:          req.Type,
			Amount:        domain.Money{AmountMinor: req.AmountMinor, Currency: req.Currency},
			TransactionID: req.TransactionID,
			BetID:         req.BetID,
			OperationID:   req.OperationID,
			ReferenceID:   req.ReferenceID,
		},
		Channel: domain.ChannelAPI,
		Actor:   principal.ClientID,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	// The stored body is written verbatim. Re-marshalling the result here would
	// risk a replay and an original differing in key order or in an added field.
	// The header is set only on a replay. Emitting `false` would make an absent
	// header and an explicit false two spellings of the same fact, and quickstart
	// V2 tells a caller to distinguish first execution from replay by the header
	// being absent.
	if out.Replay {
		w.Header().Set("X-Idempotent-Replay", "true")
	}
	if id := resultBetID(out.Body); id != "" {
		w.Header().Set("Location", "/v1/bets/"+id)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// resultBetID pulls betId out of the stored body without decoding the whole
// result, so the response itself is never re-serialized.
func resultBetID(body []byte) string {
	var probe struct {
		BetID *string `json:"betId"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.BetID == nil {
		return ""
	}
	return *probe.BetID
}

// maxRequestBytes bounds the request body. An operation is a handful of short
// fields, so anything larger is a mistake or an attack, and reading it whole
// would let a caller trade memory for latency.
const maxRequestBytes = 16 << 10

// readBody reads at most maxRequestBytes. The limit is enforced by
// MaxBytesReader, so a caller cannot make this process buffer an unbounded
// request; an oversized body is reported as a normal 400 rather than left to
// surface later as a confusing "missing field".
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *domain.Error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, domain.NewError(domain.ReasonInvalidOperationState,
				"request body exceeds the %d byte limit", maxRequestBytes)
		}
		return nil, domain.NewError(domain.ReasonMissingField, "request body could not be read")
	}
	return body, nil
}
