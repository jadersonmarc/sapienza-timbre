package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/db/migrations"
	"github.com/jadersonmarc/sapienza-timbre/internal/api"
	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/config"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/producer"
	"github.com/jadersonmarc/sapienza-timbre/internal/testutil"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// setupAttest sobe o servidor com a chave de atestação acessível e o modo de âncora dado.
// Registra a chave em attestation_keys (idempotente) e devolve o key_id + signer.
func setupAttest(t *testing.T, anchorer chain.Anchorer, anchorMode chain.AnchorMode) (*httptest.Server, *pgxpool.Pool, *ticketing.Signer, string) {
	t.Helper()
	pool := testutil.Pool(t)
	runner := tenancy.NewMigrationRunner(pool, migrations.Tenant)
	signer := ticketing.GenerateSigner()
	attestSigner := ticketing.GenerateSigner()
	// key_id único por teste (attestation_keys é global e persiste entre testes).
	keyID := "tk-" + uuid.NewString()[:8]
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO public.attestation_keys (key_id, public_key, algorithm)
		VALUES ($1,$2,'ed25519') ON CONFLICT (key_id) DO NOTHING`, keyID, attestSigner.PublicKeyB64()); err != nil {
		t.Fatalf("registrar chave: %v", err)
	}
	srv := api.NewServer(pool, auth.New("test-secret"), producer.New(pool, runner), signer, attestSigner, keyID, adminToken, "", 10*time.Minute, anchorer, anchorMode, api.Seams{
		Chain: chain.NoopChainDriver{}, Payment: payment.NewFakeGateway(), Notify: notify.NewLogNotifier(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, pool, attestSigner, keyID
}

// failAnchorer simula o relayer fora do ar.
type failAnchorer struct{}

func (failAnchorer) Enabled() bool { return true }
func (failAnchorer) SendAnchor(context.Context, []byte) (string, error) {
	return "", errors.New("relayer down")
}

// buyStanding compra n ingressos de pista e confirma; devolve o id do primeiro ticket.
func buyStanding(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string, pid uuid.UUID, n int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Attest", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, _ = getEventLots(t, ts, owner, eventID)
	body := buyViaSession(t, ts, buyer(t, ts, pool, "buy@attest.com"), map[string]any{
		"event_id": eventID, "quantity": n,
	}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))
	return firstTicket(t, ctx, pool, pid)
}

// closeEvent chama o endpoint de fechamento e devolve id + versão.
func closeEvent(t *testing.T, ts *httptest.Server, owner, eventID string) (string, int) {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/close", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("close: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	ver, _ := body["version"].(float64)
	return id, int(ver)
}

// reanchor chama o endpoint de reancoragem manual e devolve o status.
func reanchor(t *testing.T, ts *httptest.Server, owner, eventID, attID string) int {
	t.Helper()
	code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/attestations/"+attID+"/anchor", bearer(owner), nil)
	return code
}

// TestCloseGeneratesVerifiableAttestation: o fechamento gera atestado cuja assinatura
// confere com a chave pública, e o resumo é SHA-256 da serialização canônica.
func TestCloseGeneratesVerifiableAttestation(t *testing.T) {
	ts, pool, attestSigner, keyID := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa At", "owner@at.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 2)

	code, ev := do(t, ts, "GET", "/api/v1/events", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("list events: %d", code)
	}
	evs := ev["events"].([]any)
	eventID := evs[0].(map[string]any)["id"].(string)

	attID, _ := closeEvent(t, ts, owner, eventID)

	// Verificação pública SEM autenticação.
	code, pa := do(t, ts, "GET", "/api/v1/public/attestations/"+attID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("public attestation: %d", code)
	}
	digest, _ := hex.DecodeString(pa["digest"].(string))
	sig, _ := hex.DecodeString(pa["signature"].(string))
	if !ed25519.Verify(attestSigner.PublicKeyBytes(), digest, sig) {
		t.Fatalf("assinatura não confere com a chave pública")
	}
	// A chave pública retornada é a do key_id do atestado (registrada), não a corrente.
	if pa["key_id"] != keyID || pa["public_key"] != attestSigner.PublicKeyB64() {
		t.Fatalf("key_id/public_key inesperados: %v", pa["key_id"])
	}
	// Resumo = SHA-256 da serialização publicada.
	ser := pa["serialization"].(string)
	sum := sha256.Sum256([]byte(ser))
	if hex.EncodeToString(sum[:]) != pa["digest"].(string) {
		t.Fatalf("resumo não confere com SHA-256 da serialização")
	}
}

// TestAttestationDeterministic: fechar de novo (mesmo estado) é no-op e devolve o mesmo id.
func TestAttestationDeterministic(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Det", "owner@det.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	eventID := evs[0]

	id1, v1 := closeEvent(t, ts, owner, eventID)
	id2, v2 := closeEvent(t, ts, owner, eventID)
	if id1 != id2 || v1 != 1 || v2 != 1 {
		t.Fatalf("fechamento deveria ser idempotente: id1=%s v1=%d id2=%s v2=%d", id1, v1, id2, v2)
	}
}

// TestAttestationNoPersonalData: o registro canônico não carrega e-mail/nome/cpf do comprador.
func TestAttestationNoPersonalData(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Pii", "owner@pii.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	attID, _ := closeEvent(t, ts, owner, evs[0])

	code, raw := rawGet(t, ts, "/api/v1/public/attestations/"+attID, "")
	if code != http.StatusOK {
		t.Fatalf("public: %d", code)
	}
	for _, pii := range []string{"buy@attest.com", "buyer", "cpf"} {
		if strings.Contains(strings.ToLower(raw), pii) {
			t.Fatalf("registro canônico contém dado pessoal (%s): %s", pii, raw)
		}
	}
}

// TestCheckinBlockedAfterClose: check-in após o fechamento é recusado.
func TestCheckinBlockedAfterClose(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Gate", "owner@gate.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, pid, 2)
	tid1 := firstTicket(t, ctx, pool, pid)
	tid2 := lastTicket(t, ctx, pool, pid)
	tok1 := tokenOf(t, ctx, pool, pid, tid1)
	tok2 := tokenOf(t, ctx, pool, pid, tid2)
	evs := listEventIDs(t, ts, owner)

	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok1, "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("antes do fechamento: %v", vb)
	}
	closeEvent(t, ts, owner, evs[0])
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok2, "gate": "G1"}); vb["verdict"] != "invalid" {
		t.Fatalf("check-in após fechamento deveria ser recusado, veio %v", vb)
	}
}

// TestCorrectionCreatesV2: re-fechar após mudança gera versão 2 com supersedes_id.
func TestCorrectionCreatesV2(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa V2", "owner@v2.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	eventID := evs[0]
	ctx := context.Background()

	id1, _ := closeEvent(t, ts, owner, eventID)

	// Correção: insere uma cortesia diretamente (muda o agregado) e re-fecha.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var cat uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM courtesy_categories WHERE slug='convidado'`).Scan(&cat); err != nil {
			t.Fatalf("categoria: %v", err)
		}
		var ev uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM events LIMIT 1`).Scan(&ev); err != nil {
			t.Fatalf("evento: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO guest_list_entries (event_id, name, courtesy_category_id, status) VALUES ($1,'x',$2,'issued')`, ev, cat); err != nil {
			t.Fatalf("cortesia: %v", err)
		}
	})

	id2, _ := closeEvent(t, ts, owner, eventID)
	if id1 == id2 {
		t.Fatalf("correção deveria gerar nova versão")
	}
	// v1 permanece acessível; v2 a supersede.
	if code, v2 := do(t, ts, "GET", "/api/v1/public/attestations/"+id2, nil, nil); code != http.StatusOK {
		t.Fatalf("v2: %d", code)
	} else if v2["version"].(float64) != 2 || v2["supersedes_id"] != id1 {
		t.Fatalf("v2 inesperado: %v", v2)
	}
	if code, _ := do(t, ts, "GET", "/api/v1/public/attestations/"+id1, nil, nil); code != http.StatusOK {
		t.Fatalf("v1 deveria continuar acessível: %d", code)
	}
}

// TestAnchorOffClosesNormally: modo off fecha com anchor_status none.
func TestAnchorOffClosesNormally(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Off", "owner@off.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	id, _ := closeEvent(t, ts, owner, evs[0])
	code, pa := do(t, ts, "GET", "/api/v1/public/attestations/"+id, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("public: %d", code)
	}
	anchor, _ := pa["anchor"].(map[string]any)
	if anchor["status"] != "none" {
		t.Fatalf("modo off deveria ter anchor_status none, veio %v", anchor)
	}
}

// TestAnchorLogLeavesNone: em modo 'log' o fechamento deixa anchor_status none, sem
// chain_jobs e sem qualquer hash — nada vira 'anchored' sem transação real.
func TestAnchorLogLeavesNone(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.LogAnchorer{}, chain.AnchorModeLog)
	_, owner := createProducer(t, ts, "Casa Log", "owner@log.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	id, _ := closeEvent(t, ts, owner, evs[0])
	ctx := context.Background()

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs`); n != 0 {
		t.Fatalf("modo log não deveria enfileirar chain_jobs, veio %d", n)
	}
	code, pa := do(t, ts, "GET", "/api/v1/public/attestations/"+id, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("public: %d", code)
	}
	anchor, _ := pa["anchor"].(map[string]any)
	if anchor["status"] != "none" || anchor["tx_hash"] != nil {
		t.Fatalf("modo log deveria ter anchor none sem tx_hash, veio %v", anchor)
	}
}

// TestAnchorWorkerBackoffToFailed: a reancoragem enfileira; o worker tenta até maxAttempts
// com backoff antes de 'failed', persistindo o motivo — sem invalidar o atestado.
func TestAnchorWorkerBackoffToFailed(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, failAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Wk", "owner@wk.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	id, _ := closeEvent(t, ts, owner, evs[0])
	eventID := evs[0]

	// Reancoragem manual: só aceita failed|none; aqui none → 200 e vira pending.
	if code := reanchor(t, ts, owner, eventID, id); code != http.StatusOK {
		t.Fatalf("reancorar: %d", code)
	}
	if code := reanchor(t, ts, owner, eventID, id); code != http.StatusConflict {
		t.Fatalf("reancorar em pending deveria ser 409, veio %d", code)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT anchor_status FROM event_attestations WHERE id=$1`, uuid.MustParse(id)); st != "pending" {
		t.Fatalf("esperava pending após reancorar, veio %s", st)
	}

	// Worker com 3 tentativas e backoff 0: falha até esgotar, depois 'failed'.
	w := attest.NewAnchorWorker(pool, failAnchorer{})
	w.MaxAttempts = 3
	w.Backoff = func(int) time.Duration { return 0 }
	for i := 0; i < 3; i++ {
		if err := w.ProcessTenant(ctx, pid); err != nil {
			t.Fatalf("process anchor: %v", err)
		}
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT anchor_status FROM event_attestations WHERE id=$1`, uuid.MustParse(id)); st != "failed" {
		t.Fatalf("esperava failed após esgotar tentativas, veio %s", st)
	}
	if le := scanStr(t, ctx, pool, pid, `SELECT COALESCE(last_error,'') FROM chain_jobs WHERE attestation_id=$1`, uuid.MustParse(id)); le == "" {
		t.Fatalf("motivo da falha deveria estar persistido em chain_jobs")
	}
	// O atestado segue válido (digest/assinatura presentes).
	code, pa := do(t, ts, "GET", "/api/v1/public/attestations/"+id, nil, nil)
	if code != http.StatusOK || pa["digest"] == "" || pa["signature"] == "" {
		t.Fatalf("atestado inválido após falha da âncora: %d %v", code, pa)
	}
}

// TestKeyIDInRecordAndDigest: o key_id entra no registro em posição fixa e muda o resumo.
func TestKeyIDInRecordAndDigest(t *testing.T) {
	mk := func(keyID string) []byte {
		r := attest.Record{FormatVersion: 1, KeyID: keyID}
		ser, err := attest.SerializeRecord(r)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		return ser
	}
	s1, s2 := mk("a"), mk("b")
	if string(s1) == string(s2) {
		t.Fatalf("key_id diferente deveria mudar a serialização")
	}
	if !strings.Contains(string(s1), `"key_id":"a"`) {
		t.Fatalf("key_id deveria estar na serialização em posição fixa: %s", s1)
	}
	if sha256.Sum256(s1) == sha256.Sum256(s2) {
		t.Fatalf("key_id diferente deveria mudar o resumo")
	}
}

// TestKeyRotationVerification: assinado com a chave A, verifica corretamente depois que a
// chave corrente vira B (a verificação resolve pelo key_id do atestado).
func TestKeyRotationVerification(t *testing.T) {
	ts, pool, attestSigner, keyID := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Rot", "owner@rot.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	id, _ := closeEvent(t, ts, owner, evs[0])

	// Chave corrente "vira" B (nova key_id), mas A permanece no registro.
	signerB := ticketing.GenerateSigner()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO public.attestation_keys (key_id, public_key, algorithm)
		VALUES ('test-key-2',$1,'ed25519') ON CONFLICT (key_id) DO NOTHING`, signerB.PublicKeyB64()); err != nil {
		t.Fatalf("registrar chave B: %v", err)
	}

	code, pa := do(t, ts, "GET", "/api/v1/public/attestations/"+id, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("public: %d", code)
	}
	if pa["key_id"] != keyID || pa["public_key"] != attestSigner.PublicKeyB64() {
		t.Fatalf("verificação deveria usar a chave do key_id (%s), veio key_id=%v", keyID, pa["key_id"])
	}
	digest, _ := hex.DecodeString(pa["digest"].(string))
	sig, _ := hex.DecodeString(pa["signature"].(string))
	if !ed25519.Verify(attestSigner.PublicKeyBytes(), digest, sig) {
		t.Fatalf("assinatura com a chave A não confere após a corrente virar B")
	}
}

// TestRetiredKeyStillResolves: chave aposentada continua resolvendo na verificação pública.
func TestRetiredKeyStillResolves(t *testing.T) {
	ts, pool, attestSigner, keyID := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Ret", "owner@ret.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	id, _ := closeEvent(t, ts, owner, evs[0])

	if _, err := pool.Exec(context.Background(), `
		UPDATE public.attestation_keys SET retired_at=now() WHERE key_id=$1`, keyID); err != nil {
		t.Fatalf("aposentar: %v", err)
	}

	code, pa := do(t, ts, "GET", "/api/v1/public/attestations/"+id, nil, nil)
	if code != http.StatusOK || pa["public_key"] != attestSigner.PublicKeyB64() {
		t.Fatalf("chave aposentada deveria continuar resolvendo: %d %v", code, pa)
	}
	digest, _ := hex.DecodeString(pa["digest"].(string))
	sig, _ := hex.DecodeString(pa["signature"].(string))
	if !ed25519.Verify(attestSigner.PublicKeyBytes(), digest, sig) {
		t.Fatalf("chave aposentada não verifica a assinatura")
	}
}

// TestAnchorModeAndKeyValidation: relayer é rejeitado; chave sem key_id falha no config.
func TestAnchorModeAndKeyValidation(t *testing.T) {
	if chain.ValidAnchorMode("relayer") {
		t.Fatalf("relayer não é um modo aceito")
	}
	if !chain.ValidAnchorMode("off") || !chain.ValidAnchorMode("log") {
		t.Fatalf("off e log devem ser modos aceitos")
	}

	t.Setenv("DATABASE_URL", "x")
	t.Setenv("TIMBRE_JWT_SECRET", "y")
	t.Setenv("TIMBRE_ATTESTATION_KEY", "abc")
	t.Setenv("TIMBRE_ATTESTATION_KEY_ID", "")
	if _, err := config.Load(); err == nil {
		t.Fatalf("chave definida sem key_id deveria falhar no config")
	}
	t.Setenv("TIMBRE_ATTESTATION_KEY", "")
	t.Setenv("TIMBRE_ANCHOR_MODE", "relayer")
	if _, err := config.Load(); err == nil {
		t.Fatalf("anchor mode relayer deveria falhar no config")
	}
}

// TestCommitmentPercentOverflow: soma > 100% é recusada.
func TestCommitmentPercentOverflow(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Pct", "owner@pct.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	evs := listEventIDs(t, ts, owner)
	eventID := evs[0]
	cat := courtesyCategoryID(t, ctx, pool, pid, "comunidade")

	mk := func(pct string) int {
		code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/commitments", bearer(owner),
			map[string]any{"kind": "courtesy_share", "courtesy_category_id": cat, "target_type": "percent", "target_value": pct})
		return code
	}
	if mk("60") != http.StatusCreated {
		t.Fatalf("primeiro compromisso deveria passar")
	}
	if mk("60") != http.StatusBadRequest {
		t.Fatalf("soma 120%% deveria ser recusada")
	}
}

// TestCommitmentReportComparesDeclaredVsRealized: relatório compara meta vs realizado.
func TestCommitmentReportComparesDeclaredVsRealized(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Rep", "owner@rep.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, pid, 2)
	evs := listEventIDs(t, ts, owner)
	eventID := evs[0]
	cat := courtesyCategoryID(t, ctx, pool, pid, "convidado")

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/commitments", bearer(owner),
		map[string]any{"kind": "courtesy_share", "courtesy_category_id": cat, "target_type": "absolute", "target_value": "10"}); code != http.StatusCreated {
		t.Fatalf("compromisso: %d", code)
	}
	// Emite 2 cortesias (realizado 2 de 10).
	for i := 0; i < 2; i++ {
		if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner),
			map[string]any{"name": "Conv", "courtesy_category_id": cat}); code != http.StatusCreated {
			t.Fatalf("cortesia: %d", code)
		}
	}
	closeEvent(t, ts, owner, eventID)

	code, body := do(t, ts, "GET", "/api/v1/events/"+eventID+"/reports/commitments", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("relatório: %d", code)
	}
	comms, _ := body["commitments"].([]any)
	if len(comms) != 1 {
		t.Fatalf("esperava 1 compromisso, veio %v", comms)
	}
	c := comms[0].(map[string]any)
	if c["target_value"] != "10" || c["realized"] != "2" || c["status"] != "nao_cumprido" {
		t.Fatalf("comparação declarado vs realizado inesperada: %v", c)
	}
}

// TestNoMpcOrExportRoutes: nenhuma rota de carteira/exportação externa responde.
func TestNoMpcOrExportRoutes(t *testing.T) {
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa NoMpc", "owner@nompc.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = buyStanding(t, ts, pool, owner, pid, 1)
	ctx := context.Background()
	tid := firstTicket(t, ctx, pool, pid)

	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/export", bearer(owner), nil); code != http.StatusNotFound {
		t.Fatalf("export deveria ser 404, veio %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/me/tickets/"+tid.String()+"/export", bearer(verifyOTP(t, ts, pool, "buy@attest.com", "123456")), nil); code != http.StatusNotFound {
		t.Fatalf("export comprador deveria ser 404, veio %d", code)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func listEventIDs(t *testing.T, ts *httptest.Server, owner string) []string {
	t.Helper()
	code, ev := do(t, ts, "GET", "/api/v1/events", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("list events: %d", code)
	}
	var out []string
	for _, e := range ev["events"].([]any) {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

func lastTicket(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("ticket: %v", err)
		}
	})
	return id
}
