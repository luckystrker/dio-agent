// Юнит-тесты этапа 33.3 для proposal-контрактов фона: сырые управляющие
// символы внутри строк экранируются до разбора, как в TurnResult.
package background

import "testing"

func TestConsolidationProposalEscapesRawControlChars(t *testing.T) {
	raw := []byte("{\"summary\":\"говорили про\tпластинки\nи музыку\"}")
	p, err := parseConsolidationProposal(raw)
	if err != nil {
		t.Fatalf("proposal с сырым табом не разобран: %v", err)
	}
	if p.Summary != "говорили про\tпластинки\nи музыку" {
		t.Fatalf("summary повреждён: %q", p.Summary)
	}
}

func TestLifeEventProposalEscapesRawControlChars(t *testing.T) {
	raw := []byte("{\"text\":\"утром пил\tкофе\",\"mood_delta\":{\"reason\":\"спокойно\rи тепло\"}}")
	p, err := parseLifeEventProposal(raw)
	if err != nil {
		t.Fatalf("proposal события жизни с сырым табом не разобран: %v", err)
	}
	if p.Text != "утром пил кофе" || p.MoodDelta.Reason != "спокойно\rи тепло" {
		t.Fatalf("событие повреждено: %+v", p)
	}
}
