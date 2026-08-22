// Package api é a API do produto Timbre. A autenticação é NATIVA do Timbre (JWT
// próprio, validado por internal/auth) — o produtor é criado e autenticado aqui.
// Cada rota autenticada recebe as claims (colaborador + produtor + permissões
// granulares). Dados de operação por-produtor (etapas seguintes) entram por
// withTenant (search_path = tenant_<id>, public).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/producer"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// Seams reúne os drivers trocáveis (rede, pagamento, notificação). São injetados com
// defaults (Noop/stub) e consumidos pelos handlers — o padrão espelha o WhatsAppDriver da
// Margot. (O eixo de posse/MPC foi removido; a rede agora só ancora atestados.)
type Seams struct {
	Chain   chain.ChainDriver
	Payment payment.PaymentGateway
	Notify  notify.Notifier
}

// Server guarda as dependências da API.
type Server struct {
	pool         *pgxpool.Pool
	auth         *auth.Authenticator
	prov         *producer.Provisioner
	signer       *ticketing.Signer // assina ingressos (Ed25519) na emissão
	attest       *ticketing.Signer // assina atestados (chave própria, propósito distinto)
	attestKeyID     string          // identificador estável da chave de atestação
	checkoutLimits  checkout.Limits // limites/TTLs da sessão de checkout (env)
	adminToken      string          // gate do bootstrap de produtor (vazio = criação desligada)
	webhookToken string            // valida o header do webhook do Asaas (vazio = sem checagem)
	anchorer     chain.Anchorer    // envia a âncora do atestado (off = noop)
	anchorMode   chain.AnchorMode  // off | log — âncora do atestado
	seams        Seams             // drivers de rede/pagamento/notificação
	limiter      *rateLimiter      // contenção de abuso na superfície pública (§4.1)
}

// publicRateLimit é o teto por IP+rota na superfície pública. PROVISÓRIO — janela e limite
// calibráveis; o OTP tem travas próprias adicionais no banco (cooldown/tentativas).
const (
	publicRateLimit  = 60
	publicRateWindow = time.Minute
)

// NewServer constrói o servidor da API.
func NewServer(pool *pgxpool.Pool, authz *auth.Authenticator, prov *producer.Provisioner, signer, attest *ticketing.Signer, attestKeyID, adminToken, webhookToken string, checkoutLimits checkout.Limits, anchorer chain.Anchorer, anchorMode chain.AnchorMode, seams Seams) *Server {
	if anchorMode == "" {
		anchorMode = chain.AnchorModeOff
	}
	if anchorer == nil {
		anchorer = chain.NoopAnchorer{}
	}
	if checkoutLimits.MaxOpenSessionsPerIP <= 0 {
		checkoutLimits = checkout.DefaultLimits()
	}
	return &Server{
		pool: pool, auth: authz, prov: prov, signer: signer, attest: attest,
		attestKeyID: attestKeyID, checkoutLimits: checkoutLimits,
		adminToken: adminToken, webhookToken: webhookToken,
		anchorer: anchorer, anchorMode: anchorMode, seams: seams,
		limiter: newRateLimiter(publicRateLimit, publicRateWindow),
	}
}

// emitter monta o emissor (assinatura + metadados + entrega + fila on-chain) para um
// produtor.
func (s *Server) emitter(producerID uuid.UUID) checkout.Emitter {
	return checkout.Emitter{
		Signer: s.signer, Notify: s.seams.Notify, Chain: s.seams.Chain,
		ProducerID: producerID,
	}
}

// Handler devolve o mux da superfície /api/v1, embrulhado no log de acesso.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Bootstrap de produtor (aprovação de produtor vira UI de admin na Etapa 1.7).
	mux.HandleFunc("POST /api/v1/producers", s.createProducer)
	// Sessão nativa do Timbre.
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("GET /api/v1/me", s.authed(s.me))
	// Colaboradores com permissões granulares — administração é do owner.
	mux.HandleFunc("POST /api/v1/collaborators", s.requireOwner(s.createCollaborator))
	mux.HandleFunc("GET /api/v1/collaborators", s.requirePermission("relatorios", s.listCollaborators))
	// Catálogo (Etapa 1.2): escritas são do owner; leituras, de qualquer colaborador.
	mux.HandleFunc("POST /api/v1/events", s.requireOwner(s.createEvent))
	mux.HandleFunc("GET /api/v1/events", s.authed(s.listEvents))
	mux.HandleFunc("GET /api/v1/events/{id}", s.authed(s.getEvent))
	mux.HandleFunc("PATCH /api/v1/events/{id}", s.requireOwner(s.patchEvent))
	mux.HandleFunc("POST /api/v1/events/{id}/publish", s.requireOwner(s.publishEvent))
	mux.HandleFunc("POST /api/v1/events/{id}/submit-review", s.requireOwner(s.transitionEvent("pending_review")))
	mux.HandleFunc("POST /api/v1/events/{id}/suspend", s.requireOwner(s.transitionEvent("suspended")))
	mux.HandleFunc("POST /api/v1/events/{id}/cancel", s.requireOwner(s.transitionEvent("cancelled")))
	mux.HandleFunc("GET /api/v1/categories", s.authed(s.listCategories))
	mux.HandleFunc("POST /api/v1/events/{id}/lots", s.requireOwner(s.createLot))
	mux.HandleFunc("GET /api/v1/events/{id}/lots", s.authed(s.listLots))
	mux.HandleFunc("GET /api/v1/events/{id}/lots/current", s.authed(s.currentLot))
	mux.HandleFunc("POST /api/v1/venue-templates", s.requireOwner(s.createVenueTemplate))
	mux.HandleFunc("GET /api/v1/venue-templates", s.authed(s.listVenueTemplates))
	mux.HandleFunc("POST /api/v1/events/{id}/coupons", s.requireOwner(s.createCoupon))
	mux.HandleFunc("GET /api/v1/events/{id}/coupons", s.authed(s.listCoupons))
	// Inventário e mapa de assentos (Etapa 1.3): editor por grade/setores e preços.
	mux.HandleFunc("POST /api/v1/events/{id}/sectors", s.requireOwner(s.createSector))
	mux.HandleFunc("GET /api/v1/events/{id}/sectors", s.authed(s.listSectors))
	mux.HandleFunc("POST /api/v1/sectors/{id}/seats/generate", s.requireOwner(s.generateSeats))
	mux.HandleFunc("GET /api/v1/sectors/{id}/seats", s.authed(s.listSeats))
	mux.HandleFunc("POST /api/v1/seats/{id}/block", s.requireOwner(s.blockSeat))
	mux.HandleFunc("POST /api/v1/lots/{id}/prices", s.requireOwner(s.setSectorPrice))
	mux.HandleFunc("GET /api/v1/lots/{id}/prices", s.authed(s.listSectorPrices))
	// Camada pública (Onda 1): identidade do comprador por OTP (escopo próprio). Rotas
	// sensíveis com limite por IP+rota (§4.1); o OTP ainda tem cooldown/tentativas no banco.
	mux.HandleFunc("POST /api/v1/public/auth/request-code", s.rateLimited("otp-request", s.requestCode))
	mux.HandleFunc("POST /api/v1/public/auth/verify", s.rateLimited("otp-verify", s.verifyCode))
	// Camada pública (Onda 1): leitura do catálogo para o site do comprador.
	mux.HandleFunc("GET /api/v1/public/events", s.rateLimited("public-events", s.listPublicEvents))
	mux.HandleFunc("GET /api/v1/public/events/{id}", s.getPublicEvent)
	mux.HandleFunc("GET /api/v1/public/events/{id}/occupancy", s.eventOccupancy)
	mux.HandleFunc("GET /api/v1/public/categories", s.listPublicCategories)
	mux.HandleFunc("GET /api/v1/public/config", s.publicConfig)
	mux.HandleFunc("GET /api/v1/public/checkout/orders/{orderId}/status", s.checkoutStatus)
	mux.HandleFunc("GET /api/v1/public/me/tickets", s.buyerAuthed(s.myTickets))
	mux.HandleFunc("GET /api/v1/public/me", s.buyerAuthed(s.buyerMe))
	mux.HandleFunc("DELETE /api/v1/public/me", s.buyerAuthed(s.deleteMe))
	// Onda 2: posse do comprador (custódia de plataforma) — transferência e revenda.
	mux.HandleFunc("POST /api/v1/public/me/tickets/{id}/transfer", s.buyerAuthed(s.buyerTransfer))
	mux.HandleFunc("POST /api/v1/public/me/tickets/{id}/listings", s.buyerAuthed(s.buyerCreateListing))
	mux.HandleFunc("POST /api/v1/public/me/tickets/{id}/reissue", s.buyerAuthed(s.buyerReissue))
	// Camada pública (Onda 1): cadastro público de produtor (landing B2B) — self-service,
	// nasce ativo. E cadastro de artista (catálogo global), também ativo na hora.
	mux.HandleFunc("POST /api/v1/public/producer-signup", s.rateLimited("producer-signup", s.producerSignup))
	mux.HandleFunc("POST /api/v1/public/artist-signup", s.rateLimited("artist-signup", s.artistSignup))
	// Checkout (Etapa 1.4): sessão de checkout — a conta é exigida no momento de pagar.
	// A seleção e a reserva sobrevivem ao desvio de autenticação. Webhook é global.
	mux.HandleFunc("POST /api/v1/public/checkout/quote", s.rateLimited("quote", s.publicQuote))
	mux.HandleFunc("POST /api/v1/public/checkout/sessions", s.rateLimited("session-create", s.createSession))
	mux.HandleFunc("GET /api/v1/public/checkout/sessions/{id}", s.getSession)
	mux.HandleFunc("PATCH /api/v1/public/checkout/sessions/{id}", s.rateLimited("session-patch", s.patchSession))
	mux.HandleFunc("POST /api/v1/public/checkout/sessions/{id}/auth-started", s.authStarted)
	mux.HandleFunc("POST /api/v1/public/checkout/sessions/{id}/bind", s.buyerAuthed(s.bindSession))
	mux.HandleFunc("POST /api/v1/public/checkout/sessions/{id}/pay", s.buyerAuthed(s.paySession))
	mux.HandleFunc("POST /api/v1/webhooks/asaas", s.asaasWebhook)
	// Lista de convidados / cortesias — do owner.
	mux.HandleFunc("POST /api/v1/events/{id}/guests", s.requireOwner(s.createGuest))
	mux.HandleFunc("GET /api/v1/events/{id}/guests", s.authed(s.listGuests))
	// Categorias de cortesia (por produtor) — contrapartida e atestação.
	mux.HandleFunc("GET /api/v1/courtesy-categories", s.authed(s.listCourtesyCategories))
	mux.HandleFunc("POST /api/v1/courtesy-categories", s.requireOwner(s.createCourtesyCategory))
	mux.HandleFunc("PATCH /api/v1/courtesy-categories/{id}", s.requireOwner(s.patchCourtesyCategory))
	// Compromissos declarados do evento (contrapartida).
	mux.HandleFunc("POST /api/v1/events/{id}/commitments", s.requireOwner(s.createCommitment))
	mux.HandleFunc("GET /api/v1/events/{id}/commitments", s.authed(s.listCommitments))
	mux.HandleFunc("DELETE /api/v1/commitments/{id}", s.requireOwner(s.deleteCommitment))
	// Fechamento e atestação.
	mux.HandleFunc("POST /api/v1/events/{id}/close", s.requireOwner(s.closeEvent))
	mux.HandleFunc("POST /api/v1/events/{id}/attestations/{attId}/anchor", s.requireOwner(s.reanchorEvent))
	mux.HandleFunc("GET /api/v1/events/{id}/reports/audience", s.requirePermission("relatorios", s.audienceReport))
	mux.HandleFunc("GET /api/v1/events/{id}/reports/commitments", s.requirePermission("relatorios", s.commitmentsReport))
	// Verificação pública do atestado (sem auth).
	mux.HandleFunc("GET /api/v1/public/attestations/{id}", s.publicAttestation)
	// Portaria (Etapa 1.6): validação e sync exigem a permissão granular 'checkin'.
	mux.HandleFunc("GET /api/v1/gate/config", s.requirePermission("checkin", s.gateConfig))
	mux.HandleFunc("GET /api/v1/gate/events/{id}/seatmap", s.requirePermission("checkin", s.gateSeatmap))
	mux.HandleFunc("POST /api/v1/gate/validate", s.requirePermission("checkin", s.gateValidate))
	mux.HandleFunc("POST /api/v1/gate/sync", s.requirePermission("checkin", s.gateSync))
	// Painel do produtor (Etapa 1.7) — permissão relatorios.
	mux.HandleFunc("GET /api/v1/dash/summary", s.requirePermission("relatorios", s.dashSummary))
	mux.HandleFunc("GET /api/v1/dash/events/{id}", s.requirePermission("relatorios", s.dashOverview))
	mux.HandleFunc("GET /api/v1/dash/events/{id}/export.csv", s.requirePermission("relatorios", s.dashExportCSV))
	mux.HandleFunc("GET /api/v1/dash/payouts", s.requirePermission("relatorios", s.dashPayouts))
	mux.HandleFunc("GET /api/v1/dash/events/{id}/notifications", s.requirePermission("relatorios", s.dashNotifications))
	mux.HandleFunc("POST /api/v1/dash/notifications/{id}/resend", s.requireOwner(s.resendNotification))
	// Transferência restrita (Etapa 2.1) — por ora operada pelo owner.
	mux.HandleFunc("POST /api/v1/tickets/{id}/transfer", s.requireOwner(s.transferTicket))
	// Mercado secundário (Etapa 2.2): anúncio/cancelamento e procedência (owner);
	// compra é do comprador autenticado.
	mux.HandleFunc("POST /api/v1/tickets/{id}/listings", s.requireOwner(s.createListing))
	mux.HandleFunc("GET /api/v1/tickets/{id}/provenance", s.requirePermission("relatorios", s.provenance))
	mux.HandleFunc("POST /api/v1/listings/{id}/cancel", s.requireOwner(s.cancelListing))
	mux.HandleFunc("POST /api/v1/public/listings/{id}/buy", s.buyerAuthed(s.buyListing))
	// Passe de temporada (Etapa 2.3): gestão do owner; compra do comprador autenticado.
	mux.HandleFunc("POST /api/v1/season-passes", s.requireOwner(s.createPass))
	mux.HandleFunc("POST /api/v1/season-passes/{id}/dates", s.requireOwner(s.addPassDate))
	mux.HandleFunc("POST /api/v1/public/season-passes/{id}/buy", s.buyerAuthed(s.buyPass))
	// Panorama de passeios (Etapa 2.5) — peça pública compartilhável do público.
	mux.HandleFunc("GET /api/v1/public/subjects/{id}/panorama", s.subjectPanorama)
	// Descoberta e confiança (Etapa 2.6): avaliação restrita a quem fez check-in,
	// reputação verificável e descoberta por presença real. Tudo público.
	// Gestão do ingresso como NFT (Etapa 1.9): metadados/estado públicos; export/disputa/
	// reemissão pelo owner (autonomia do participante vem com a identidade).
	mux.HandleFunc("GET /api/v1/public/tokens/{id}/metadata", s.tokenMetadata)
	mux.HandleFunc("GET /api/v1/public/tokens/{id}", s.tokenView)
	mux.HandleFunc("POST /api/v1/tickets/{id}/dispute", s.requireOwner(s.disputeTicket))
	mux.HandleFunc("POST /api/v1/tickets/{id}/reissue", s.requireOwner(s.reissueTicket))
	mux.HandleFunc("POST /api/v1/public/checkins/{id}/review", s.submitReview)
	mux.HandleFunc("GET /api/v1/public/producers/{id}/reputation", s.producerReputation)
	mux.HandleFunc("GET /api/v1/public/subjects/{id}/discovery", s.subjectDiscovery)
	// Painel administrativo (plataforma) — JWT de admin (escopo "admin"). O X-Admin-Token
	// ficou restrito ao bootstrap (POST /producers). Aprovação manual virou exceção
	// (self-service); o approve fica para reativar produtor suspenso.
	mux.HandleFunc("POST /api/v1/admin/login", s.adminLogin)
	mux.HandleFunc("GET /api/v1/admin/me", s.requireAdmin(s.adminMe))
	mux.HandleFunc("GET /api/v1/admin/summary", s.requireAdmin(s.adminSummary))
	mux.HandleFunc("GET /api/v1/admin/producers", s.requireAdmin(s.listAdminProducers))
	mux.HandleFunc("POST /api/v1/admin/producers/{id}/approve", s.requireAdmin(s.adminSetProducerStatus("active")))
	mux.HandleFunc("POST /api/v1/admin/producers/{id}/suspend", s.requireAdmin(s.adminSetProducerStatus("suspended")))
	mux.HandleFunc("POST /api/v1/admin/producers/{pid}/events/{eid}/suspend", s.requireAdmin(s.adminSuspendEvent))
	// Catálogo global de artistas (admin) e visão consolidada de eventos.
	mux.HandleFunc("GET /api/v1/admin/artists", s.requireAdmin(s.listArtists))
	mux.HandleFunc("POST /api/v1/admin/artists", s.requireAdmin(s.createArtist))
	mux.HandleFunc("PATCH /api/v1/admin/artists/{id}", s.requireAdmin(s.patchArtist))
	mux.HandleFunc("POST /api/v1/admin/artists/{id}/status", s.requireAdmin(s.setArtistStatus))
	mux.HandleFunc("GET /api/v1/admin/events", s.requireAdmin(s.listAdminEvents))
	// Moderação (reativa), auditoria e relatórios consolidados.
	mux.HandleFunc("GET /api/v1/admin/moderation/queue", s.requireAdmin(s.moderationQueue))
	mux.HandleFunc("PATCH /api/v1/admin/moderation/{id}", s.requireAdmin(s.resolveModeration))
	mux.HandleFunc("GET /api/v1/admin/audit-log", s.requireAdmin(s.auditLog))
	mux.HandleFunc("GET /api/v1/admin/reports/sales", s.requireAdmin(s.reportsSales))
	// Gestão de operadores — só super_admin.
	mux.HandleFunc("GET /api/v1/admin/admins", s.requireSuperAdmin(s.listAdmins))
	mux.HandleFunc("POST /api/v1/admin/admins", s.requireSuperAdmin(s.createAdmin))
	mux.HandleFunc("PATCH /api/v1/admin/admins/{id}", s.requireSuperAdmin(s.setAdminRole))
	// Denúncia pública (moderação reativa) — com limite por IP+rota.
	mux.HandleFunc("POST /api/v1/public/moderation/flags", s.rateLimited("flag", s.createModerationFlag))
	// Programa de produtores (Etapa 2.7): nível e originação (admin); nível/extrato (produtor).
	mux.HandleFunc("POST /api/v1/admin/producers/{id}/tier", s.requireAdmin(s.adminSetTier))
	mux.HandleFunc("POST /api/v1/admin/producers/{id}/origination", s.requireAdmin(s.adminSetOrigination))
	mux.HandleFunc("GET /api/v1/dash/program", s.requirePermission("relatorios", s.dashProgram))
	mux.HandleFunc("GET /api/v1/dash/origination", s.requirePermission("relatorios", s.dashOrigination))
	// Divulgação e engajamento (Etapa 2.8): campanhas/audiência (owner/relatorios) e
	// links/lista de espera/pixels (público).
	mux.HandleFunc("POST /api/v1/events/{id}/campaigns", s.requireOwner(s.createCampaign))
	mux.HandleFunc("GET /api/v1/events/{id}/campaigns", s.requirePermission("relatorios", s.listCampaigns))
	mux.HandleFunc("GET /api/v1/dash/events/{id}/audience", s.requirePermission("relatorios", s.audienceProfile))
	mux.HandleFunc("POST /api/v1/public/campaigns/{id}/click", s.campaignClick)
	mux.HandleFunc("POST /api/v1/public/events/{id}/waitlist", s.joinWaitlist)
	mux.HandleFunc("GET /api/v1/public/events/{id}/pixels", s.eventPixels)
	// Audiência (Fase 3): segmentos/patrocinadores (admin/plataforma); consentimento do
	// público (público). Guardrail: patrocinador nunca vê identidade.
	mux.HandleFunc("POST /api/v1/admin/segments", s.requireAdmin(s.createSegment))
	mux.HandleFunc("POST /api/v1/admin/segments/{id}/recompute", s.requireAdmin(s.recomputeSegment))
	mux.HandleFunc("POST /api/v1/admin/sponsor-campaigns", s.requireAdmin(s.createSponsorCampaign))
	mux.HandleFunc("POST /api/v1/admin/sponsor-campaigns/{id}/deliver", s.requireAdmin(s.deliverCampaign))
	mux.HandleFunc("GET /api/v1/admin/sponsor-campaigns/{id}/metrics", s.requireAdmin(s.campaignMetrics))
	mux.HandleFunc("POST /api/v1/public/subjects/{id}/consents", s.setConsent)
	mux.HandleFunc("GET /api/v1/public/subjects/{id}/consents", s.listConsents)
	return secureHeaders(accessLog(mux))
}

// ── middleware ───────────────────────────────────────────────────────────────

type authedHandler func(w http.ResponseWriter, r *http.Request, claims *auth.Claims)

// authed valida o JWT do Timbre e injeta as claims. Sem token válido → 401.
func (s *Server) authed(fn authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		claims, err := s.auth.Verify(tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		fn(w, r, claims)
	}
}

// requirePermission exige uma permissão granular (o owner passa sempre).
func (s *Server) requirePermission(perm string, fn authedHandler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
		if !claims.Has(perm) {
			writeErr(w, http.StatusForbidden, "requer permissão "+perm)
			return
		}
		fn(w, r, claims)
	})
}

// requireOwner exige que o colaborador seja owner do produtor.
func (s *Server) requireOwner(fn authedHandler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
		if !claims.Owner {
			writeErr(w, http.StatusForbidden, "requer owner")
			return
		}
		fn(w, r, claims)
	})
}

// withTenant roda fn numa transação escopada ao schema do produtor. Base para os
// handlers de operação das próximas etapas (catálogo, venda, ...).
func (s *Server) withTenant(ctx context.Context, producerID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, producerID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── handlers ─────────────────────────────────────────────────────────────────

type createProducerReq struct {
	Name          string `json:"name"`
	OwnerEmail    string `json:"owner_email"`
	OwnerPassword string `json:"owner_password"`
}

// createProducer cria um produtor + owner e provisiona o schema. Gate por token de
// admin no header X-Admin-Token (bootstrap; a aprovação de produtor vem na 1.7).
func (s *Server) createProducer(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" {
		writeErr(w, http.StatusServiceUnavailable, "criação de produtor desligada (defina TIMBRE_ADMIN_TOKEN)")
		return
	}
	if !subtleCompare(r.Header.Get("X-Admin-Token"), s.adminToken) {
		writeErr(w, http.StatusUnauthorized, "token de admin inválido")
		return
	}
	var body createProducerReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.OwnerEmail) == "" || len(body.OwnerPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "name, owner_email e owner_password (mín. 8) obrigatórios")
		return
	}
	res, err := s.prov.Create(r.Context(), body.Name, body.OwnerEmail, body.OwnerPassword)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "e-mail já usado neste produtor")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// login confere e-mail+senha e devolve um JWT do Timbre. O mesmo e-mail pode existir
// em produtores diferentes; conferimos a senha em cada candidato.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body loginReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	cands, err := store.FindCollaboratorsByEmail(r.Context(), s.pool, email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, c := range cands {
		if !auth.ComparePassword(c.PasswordHash, body.Password) {
			continue
		}
		perms, err := store.ListPermissions(r.Context(), s.pool, c.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		tok, err := s.auth.Issue(auth.Identity{
			CollaboratorID: c.ID, ProducerID: c.ProducerID, Owner: c.IsOwner,
			SessionVersion: c.SessionVersion, Permissions: perms,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": tok})
		return
	}
	writeErr(w, http.StatusUnauthorized, "credenciais inválidas")
}

// me devolve o colaborador autenticado + produtor + permissões (prova a auth).
func (s *Server) me(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	cid, err := claims.CollaboratorID()
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "token inválido")
		return
	}
	collab, err := store.GetCollaborator(r.Context(), s.pool, cid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	prod, err := store.GetProducer(r.Context(), s.pool, collab.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	perms, err := store.ListPermissions(r.Context(), s.pool, cid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"collaborator": collab,
		"producer":     prod,
		"permissions":  perms,
	})
}

type createCollaboratorReq struct {
	Email       string   `json:"email"`
	Password    string   `json:"password"`
	Permissions []string `json:"permissions"`
}

// createCollaborator convida um colaborador com permissões granulares (owner only).
func (s *Server) createCollaborator(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body createCollaboratorReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if strings.TrimSpace(body.Email) == "" || len(body.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "email e password (mín. 8) obrigatórios")
		return
	}
	for _, p := range body.Permissions {
		if !auth.ValidPermission(p) {
			writeErr(w, http.StatusBadRequest, "permissão inválida: "+p)
			return
		}
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))

	var collab store.Collaborator
	err = pgxTx(r.Context(), s.pool, func(tx pgx.Tx) error {
		c, err := store.CreateCollaborator(r.Context(), tx, claims.ProducerID, email, hash, false)
		if err != nil {
			return err
		}
		for _, p := range body.Permissions {
			if err := store.AddPermission(r.Context(), tx, c.ID, p); err != nil {
				return err
			}
		}
		collab = c
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "e-mail já usado neste produtor")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"collaborator": collab,
		"permissions":  body.Permissions,
	})
}

// listCollaborators lista os colaboradores do produtor (owner ou permissão relatorios).
func (s *Server) listCollaborators(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	list, err := store.ListCollaborators(r.Context(), s.pool, claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"collaborators": list})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// pgxTx roda fn numa transação simples (sem escopo de tenant), para escritas no
// control plane em `public`.
func pgxTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
}

// parseTimePtr converte um ponteiro de string RFC3339 em *time.Time (nil se vazio).
func parseTimePtr(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func bearer(r *http.Request) string {
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// subtleCompare compara dois tokens em tempo constante.
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// accessLog registra cada requisição com um request-id e a duração (observabilidade
// mínima desta etapa).
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := uuid.NewString()
		w.Header().Set("X-Request-Id", reqID)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("request",
			"id", reqID, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
