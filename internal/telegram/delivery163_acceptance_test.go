// Приёмочные юнит-тесты пункта 16.3 (тестировщик, свежий контекст), без БД:
// дефолт RecoveryInterval из ТЗ (30 с) зафиксирован тестом — молчаливый сдвиг
// дефолта меняет частоту recovery-запросов в живом такте.
package telegram

import (
	"testing"
	"time"
)

// TestDeliveryOptionsRecoveryIntervalDefault — 16.3(б): интервал
// периодического recovery — параметр DeliveryOptions, дефолт 30 с; явное
// значение не перезаписывается дефолтом.
func TestDeliveryOptionsRecoveryIntervalDefault(t *testing.T) {
	if got := (DeliveryOptions{}).withDefaults().RecoveryInterval; got != 30*time.Second {
		t.Fatalf("дефолт RecoveryInterval = %v, хочу 30с (ТЗ 16.3)", got)
	}
	explicit := DeliveryOptions{RecoveryInterval: 5 * time.Second}.withDefaults()
	if explicit.RecoveryInterval != 5*time.Second {
		t.Fatalf("явный RecoveryInterval перезаписан: %v, хочу 5с", explicit.RecoveryInterval)
	}
}
