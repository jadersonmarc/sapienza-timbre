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
