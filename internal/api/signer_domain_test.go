package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

const testPrivateKey = "4f3edf983ac636a65a842ce7c78d9aa706d3b113bce036f1132f8fdbf58f4f6f"

func TestSigner_DomainSelectionByToken(t *testing.T) {
	signer, err := NewSigner(testPrivateKey)
	if err != nil {
		t.Fatalf("NewSigner failed: %v", err)
	}
	signer.SetNegRiskTokenIDs([]string{"0xneg-risk-token"})

	regular := signer.getDomainSeparator("0xregular-token")
	negRisk := signer.getDomainSeparator("0xneg-risk-token")

	expectedRegular := domainSeparatorForContract(regularExchangeVerifyingContract)
	expectedNegRisk := domainSeparatorForContract(negRiskExchangeVerifyingContract)

	if regular != expectedRegular {
		t.Fatalf("regular token should use regular exchange domain")
	}
	if negRisk != expectedNegRisk {
		t.Fatalf("neg-risk token should use neg-risk exchange domain")
	}
}

func TestSigner_SignatureRecoversWithSelectedDomain(t *testing.T) {
	signer, err := NewSigner(testPrivateKey)
	if err != nil {
		t.Fatalf("NewSigner failed: %v", err)
	}
	signer.SetNegRiskTokenIDs([]string{"0xneg-risk-token"})

	order := &OrderData{
		Salt:          "1",
		Maker:         signer.Address(),
		Signer:        signer.Address(),
		Taker:         "0x0000000000000000000000000000000000000000",
		TokenID:       "0xneg-risk-token",
		MakerAmount:   "2000000",
		TakerAmount:   "980000",
		Expiration:    "0",
		Nonce:         "0",
		FeeRateBps:    "0",
		Side:          1,
		SignatureType: 0,
	}

	sigHex, err := signer.SignOrder(order)
	if err != nil {
		t.Fatalf("SignOrder failed: %v", err)
	}

	recovered, err := recoverSigner(sigHex, domainSeparatorForContract(negRiskExchangeVerifyingContract), signer.getOrderStructHash(order))
	if err != nil {
		t.Fatalf("recoverSigner failed: %v", err)
	}
	if !strings.EqualFold(recovered, signer.Address()) {
		t.Fatalf("signature did not recover to signer under neg-risk domain")
	}

	wrongDomainRecovered, err := recoverSigner(sigHex, domainSeparatorForContract(regularExchangeVerifyingContract), signer.getOrderStructHash(order))
	if err == nil && strings.EqualFold(wrongDomainRecovered, signer.Address()) {
		t.Fatalf("signature should not validate under regular domain for neg-risk token")
	}
}

func TestCLOBClient_PlaceOrderSellRatioAcceptedWithCorrectDomain(t *testing.T) {
	clob, err := NewCLOBClient(testPrivateKey, "owner", "secret", "pass")
	if err != nil {
		t.Fatalf("NewCLOBClient failed: %v", err)
	}

	negRiskToken := "0xneg-risk-token"
	clob.GetSigner().SetNegRiskTokenIDs([]string{negRiskToken})

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/order" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		defer r.Body.Close()

		var payload struct {
			Order OrderPayload `json:"order"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad json"}`))
			return
		}

		if payload.Order.MakerAmount != "2000000" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unexpected makerAmount"}`))
			return
		}
		if payload.Order.TakerAmount != "980000" && payload.Order.TakerAmount != "1000000" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unexpected takerAmount"}`))
			return
		}

		orderForHash := &OrderData{
			Salt:          strconv.FormatInt(payload.Order.Salt, 10),
			Maker:         payload.Order.Maker,
			Signer:        payload.Order.Signer,
			Taker:         payload.Order.Taker,
			TokenID:       payload.Order.TokenID,
			MakerAmount:   payload.Order.MakerAmount,
			TakerAmount:   payload.Order.TakerAmount,
			Expiration:    payload.Order.Expiration,
			Nonce:         payload.Order.Nonce,
			FeeRateBps:    payload.Order.FeeRateBps,
			SignatureType: payload.Order.SignatureType,
		}
		if payload.Order.Side == "1" {
			orderForHash.Side = 1
		}

		recovered, err := recoverSigner(payload.Order.Signature, domainSeparatorForContract(negRiskExchangeVerifyingContract), clob.GetSigner().getOrderStructHash(orderForHash))
		if err != nil || !strings.EqualFold(recovered, clob.Address()) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad signature"}`))
			return
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"orderID":"ok","status":"LIVE"}`))
	})

	server := httptest.NewServer(h)
	defer server.Close()
	clob.BaseURL = server.URL

	for _, tc := range []struct {
		name  string
		price float64
	}{
		{name: "ratio_0_98", price: 0.49},
		{name: "ratio_1_00", price: 0.50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := clob.PlaceOrder(context.Background(), &OrderRequest{
				TokenID:     negRiskToken,
				Price:       tc.price,
				Size:        2.0,
				Side:        SideSell,
				OrderType:   OrderTypeMarket,
				TimeInForce: TIFFillOrKill,
			})
			if err != nil {
				t.Fatalf("PlaceOrder failed: %v", err)
			}
			if !resp.Success {
				t.Fatalf("expected success, got response: %+v", resp)
			}
		})
	}
}

func domainSeparatorForContract(contract string) [32]byte {
	typeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	nameHash := keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := keccak256([]byte("1"))
	chainID := big.NewInt(137)
	contractAddr := parseAddress(contract)

	encoded := make([]byte, 32*5)
	copy(encoded[0:32], typeHash[:])
	copy(encoded[32:64], nameHash[:])
	copy(encoded[64:96], versionHash[:])
	copy(encoded[96:128], padLeft(chainID.Bytes(), 32))
	copy(encoded[128:160], padLeft(contractAddr, 32))
	return keccak256(encoded)
}

func recoverSigner(signatureHex string, domainSeparator [32]byte, structHash [32]byte) (string, error) {
	sig, err := hex.DecodeString(strings.TrimPrefix(signatureHex, "0x"))
	if err != nil {
		return "", err
	}
	if len(sig) != 65 {
		return "", io.ErrUnexpectedEOF
	}
	sig = bytes.Clone(sig)
	sig[64] -= 27

	message := make([]byte, 2+32+32)
	message[0], message[1] = 0x19, 0x01
	copy(message[2:34], domainSeparator[:])
	copy(message[34:66], structHash[:])
	h := keccak256(message)

	pub, err := crypto.SigToPub(h[:], sig)
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(*pub).Hex(), nil
}
