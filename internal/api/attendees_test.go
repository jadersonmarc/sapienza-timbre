package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestTicketsCarryAttendee: a ficha nominal preenchida no checkout chega ao ingresso. Sem
// isso, quatro entradas do mesmo pedido são indistinguíveis e a portaria não tem o que
// conferir com o documento.
func TestTicketsCarryAttendee(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Att", "owner@att.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Att", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	hdr := buyer(t, ts, pool, "ficha@att.com")
	code, sess := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
		map[string]any{"event_id": eventID, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("sessão: %d %v", code, sess)
	}
	sid := sess["id"].(string)
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/bind", hdr, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	attendees := []map[string]any{
		{"name": "Maria Souza", "cpf": testCPF("maria@att.com")},
		{"name": "João Lima", "cpf": testCPF("joao@att.com")},
	}
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr,
		map[string]any{"method": "pix", "attendees": attendees})
	if code != http.StatusCreated {
		t.Fatalf("pay: %d %v", code, body)
	}
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	names := map[string]bool{}
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT COALESCE(attendee_name,''), COALESCE(attendee_cpf,'') FROM tickets`)
		if err != nil {
			t.Fatalf("ler ingressos: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, cpf string
			if err := rows.Scan(&name, &cpf); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if cpf == "" {
				t.Fatalf("ingresso sem documento do participante (%q)", name)
			}
			names[name] = true
		}
	})
	if !names["Maria Souza"] || !names["João Lima"] {
		t.Fatalf("esperava os dois participantes nomeados, veio %v", names)
	}
}

// TestAttendeeValidation: ficha incompleta, documento inválido ou repetido não passa. O
// mesmo CPF em dois ingressos do pedido transformaria meia-entrada em vale de revenda.
func TestAttendeeValidation(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Val", "owner@val.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Val", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	hdr := buyer(t, ts, pool, "val@val.com")
	valid := testCPF("unico@val.com")

	cases := []struct {
		nome      string
		attendees []map[string]any
	}{
		{"quantidade divergente", []map[string]any{{"name": "Ana Paula", "cpf": valid}}},
		{"documento inválido", []map[string]any{
			{"name": "Ana Paula", "cpf": "11111111111"},
			{"name": "Bruno Dias", "cpf": valid},
		}},
		{"nome incompleto", []map[string]any{
			{"name": "Ana", "cpf": valid},
			{"name": "Bruno Dias", "cpf": testCPF("bruno@val.com")},
		}},
		{"documento repetido", []map[string]any{
			{"name": "Ana Paula", "cpf": valid},
			{"name": "Bruno Dias", "cpf": valid},
		}},
	}
	for _, c := range cases {
		code, sess := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
			map[string]any{"event_id": eventID, "quantity": 2, "anon_token": uuid.NewString()})
		if code != http.StatusCreated {
			t.Fatalf("%s: sessão %d", c.nome, code)
		}
		sid := sess["id"].(string)
		if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/bind", hdr, nil); code != http.StatusOK {
			t.Fatalf("%s: bind %d", c.nome, code)
		}
		code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr,
			map[string]any{"method": "pix", "attendees": c.attendees})
		if code != http.StatusBadRequest {
			t.Fatalf("%s: esperava 400, veio %d %v", c.nome, code, body)
		}
	}
}

// TestSessionFollowsNewSelection: voltar e trocar a quantidade tem que valer. A sessão era
// retomada pelo anon_token ignorando a seleção enviada — quem escolhia 2, voltava e
// escolhia 1, pagava 2.
func TestSessionFollowsNewSelection(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Sel", "owner@sel.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Sel", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	anon := uuid.NewString()

	code, first := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
		map[string]any{"event_id": eventID, "quantity": 2, "anon_token": anon})
	if code != http.StatusCreated {
		t.Fatalf("primeira sessão: %d %v", code, first)
	}
	code, second := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
		map[string]any{"event_id": eventID, "quantity": 1, "anon_token": anon})
	if code != http.StatusCreated {
		t.Fatalf("segunda sessão: %d %v", code, second)
	}
	if second["id"] != first["id"] {
		t.Fatalf("o mesmo navegador deveria retomar a sessão, veio outra")
	}
	items := second["items"].(map[string]any)
	if q := int(items["quantity"].(float64)); q != 1 {
		t.Fatalf("a sessão deveria refletir a nova seleção (1), veio %d", q)
	}

	// E a reserva acompanha: o pagamento cobra uma entrada, não duas.
	hdr := buyer(t, ts, pool, "sel@sel.com")
	sid := second["id"].(string)
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/bind", hdr, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	code, pay := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr, map[string]any{"method": "pix"})
	if code != http.StatusCreated {
		t.Fatalf("pay: %d %v", code, pay)
	}
	if face := int64(pay["face_cents"].(float64)); face != 5000 {
		t.Fatalf("esperava cobrar uma entrada (5000), veio %d", face)
	}
}

// TestHalfPricePerAttendee: a meia é de quem entra, não do pedido. O participante marcado
// paga metade, o ingresso dele nasce marcado (a portaria cobra o comprovante dele) e a
// contagem da ficha tem que bater com a seleção.
func TestHalfPricePerAttendee(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Meia", "owner@meia.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Meia", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 6000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	hdr := buyer(t, ts, pool, "meia@meia.com")

	newSession := func(anon string) string {
		code, sess := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
			map[string]any{"event_id": eventID, "quantity": 2, "half_price_qty": 1, "anon_token": anon})
		if code != http.StatusCreated {
			t.Fatalf("sessão: %d %v", code, sess)
		}
		sid := sess["id"].(string)
		if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/bind", hdr, nil); code != http.StatusOK {
			t.Fatalf("bind: %d", code)
		}
		return sid
	}

	// Ficha que não bate com a seleção (nenhuma meia marcada) é recusada.
	sid := newSession(uuid.NewString())
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr, map[string]any{
		"method": "pix",
		"attendees": []map[string]any{
			{"name": "Ana Paula", "cpf": testCPF("ana@meia.com")},
			{"name": "Bruno Dias", "cpf": testCPF("bruno@meia.com")},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("ficha sem a meia declarada deveria ser recusada, veio %d %v", code, body)
	}

	// Ficha correta: a segunda pessoa é a meia.
	sid = newSession(uuid.NewString())
	code, body = do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr, map[string]any{
		"method": "pix",
		"attendees": []map[string]any{
			{"name": "Ana Paula", "cpf": testCPF("ana@meia.com")},
			{"name": "Bruno Dias", "cpf": testCPF("bruno@meia.com"), "half_price": true},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("pay: %d %v", code, body)
	}
	// Uma inteira (6000) + uma meia (3000).
	if face := int64(body["face_cents"].(float64)); face != 9000 {
		t.Fatalf("esperava 9000 (uma inteira + uma meia), veio %d", face)
	}
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	half := map[string]bool{}
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT COALESCE(attendee_name,''), half_price FROM tickets`)
		if err != nil {
			t.Fatalf("ler ingressos: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var h bool
			if err := rows.Scan(&name, &h); err != nil {
				t.Fatalf("scan: %v", err)
			}
			half[name] = h
		}
	})
	if !half["Bruno Dias"] {
		t.Fatalf("o ingresso do participante com meia deveria estar marcado: %v", half)
	}
	if half["Ana Paula"] {
		t.Fatalf("o ingresso inteiro não pode aparecer como meia: %v", half)
	}
}

// TestResetPassword: quem esqueceu a senha define uma nova com o código do e-mail, e o
// código não serve duas vezes. Antes o código só deixava entrar — a senha continuava
// perdida e toda volta dependia de outro e-mail.
func TestResetPassword(t *testing.T) {
	ts, pool := setup(t)
	const email = "esqueci@reset.com"
	_ = buyer(t, ts, pool, email)

	// Código conhecido, plantado como o pedido por e-mail plantaria.
	const otp = "778899"
	insertOTP(t, pool, email, otp)

	// Senha curta é recusada antes de queimar o código.
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/reset-password", nil,
		map[string]any{"email": email, "code": otp, "password": "123"}); code != http.StatusBadRequest {
		t.Fatalf("senha curta deveria ser recusada, veio %d", code)
	}
	code, body := do(t, ts, "POST", "/api/v1/public/auth/reset-password", nil,
		map[string]any{"email": email, "code": otp, "password": "outra-senha-9"})
	if code != http.StatusOK {
		t.Fatalf("reset: %d %v", code, body)
	}
	if body["token"] == nil {
		t.Fatalf("o reset deveria já deixar a pessoa dentro: %v", body)
	}

	// A senha nova entra; a antiga não.
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/login", nil,
		map[string]any{"email": email, "password": "outra-senha-9"}); code != http.StatusOK {
		t.Fatalf("login com a senha nova: %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/login", nil,
		map[string]any{"email": email, "password": "senha-forte-1"}); code != http.StatusUnauthorized {
		t.Fatalf("a senha antiga não deveria mais valer, veio %d", code)
	}
	// Código é de uso único.
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/reset-password", nil,
		map[string]any{"email": email, "code": otp, "password": "terceira-senha-9"}); code != http.StatusUnauthorized {
		t.Fatalf("código reusado deveria falhar, veio %d", code)
	}
}

// TestMaskedCPFAccepted: o documento chega da tela como foi digitado — com pontos e traço.
// Usá-lo cru mandava a pontuação ao gateway e à conta, e o erro voltava como "CPF inválido"
// para um documento correto.
func TestMaskedCPFAccepted(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Masc", "owner@masc.com", "senha1234")
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Masc", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 4000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	// Conta sem CPF (criada pelo código, como as antigas): o documento vem no pagamento.
	hdr := bearer(verifyOTP(t, ts, pool, "mascara@masc.com", "246810"))

	code, sess := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
		map[string]any{"event_id": eventID, "quantity": 1, "anon_token": uuid.NewString()})
	if code != http.StatusCreated {
		t.Fatalf("sessão: %d %v", code, sess)
	}
	sid := sess["id"].(string)
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/bind", hdr, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr,
		map[string]any{"method": "pix", "buyer_cpf": "111.444.777-35"})
	if code != http.StatusCreated {
		t.Fatalf("CPF mascarado deveria ser aceito, veio %d %v", code, body)
	}
	var saved string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(cpf,'') FROM subjects WHERE lower(email)='mascara@masc.com'`).Scan(&saved); err != nil {
		t.Fatalf("ler conta: %v", err)
	}
	if saved != "11144477735" {
		t.Fatalf("a conta deveria guardar só os dígitos, veio %q", saved)
	}

	// E documento realmente inválido continua sendo recusado, com mensagem clara.
	code, sess = do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
		map[string]any{"event_id": eventID, "quantity": 1, "anon_token": uuid.NewString()})
	if code != http.StatusCreated {
		t.Fatalf("sessão 2: %d", code)
	}
	sid = sess["id"].(string)
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/bind", hdr, nil); code != http.StatusOK {
		t.Fatalf("bind 2: %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+sid+"/pay", hdr,
		map[string]any{"method": "pix", "buyer_cpf": "111.444.777-34"}); code != http.StatusBadRequest {
		t.Fatalf("CPF inválido deveria ser recusado, veio %d", code)
	}
}
