package dex

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/yourusername/screner/internal/util"
)

// PoolToken описывает токен внутри пула и служит идентификатором в составе ключа.
type PoolToken struct {
	Address common.Address
	Symbol  string
}

// Identifier возвращает нормализованный идентификатор токена (адрес или символ).
func (t PoolToken) Identifier() string {
	if t.Address != (common.Address{}) {
		return strings.ToLower(strings.TrimSpace(t.Address.Hex()))
	}
	return strings.ToLower(strings.TrimSpace(t.Symbol))
}

// PoolDescriptor задаёт единый контракт пула: dex + amm_version + network + токены.
type PoolDescriptor struct {
	Dex        string
	AMMVersion string
	Network    string
	Token0     PoolToken
	Token1     PoolToken
}

// CompositeKey формирует нормализованный ключ для пула (dex|amm|network|token0|token1).
func (p PoolDescriptor) CompositeKey() string {
	dex := strings.ToLower(strings.TrimSpace(p.Dex))
	amm := normalizeAMM(p.AMMVersion)
	network := normalizeNetwork(p.Network)
	return fmt.Sprintf("%s|%s|%s|%s|%s", dex, amm, network, p.Token0.Identifier(), p.Token1.Identifier())
}

// Validate проверяет заполненность обязательных полей пула и логирует результат.
func (p PoolDescriptor) Validate() error {
	util.Debugf("pool_descriptor: validate dex=%s amm=%s network=%s", p.Dex, p.AMMVersion, p.Network)
	if strings.TrimSpace(p.Dex) == "" {
		err := errors.New("pool descriptor: empty dex")
		util.Errorf(err.Error())
		return err
	}
	if normalizeAMM(p.AMMVersion) == "" {
		err := errors.New("pool descriptor: empty amm_version")
		util.Errorf(err.Error())
		return err
	}
	if normalizeNetwork(p.Network) == "" {
		err := errors.New("pool descriptor: empty network")
		util.Errorf(err.Error())
		return err
	}
	if p.Token0.Identifier() == "" {
		err := errors.New("pool descriptor: empty token0")
		util.Errorf(err.Error())
		return err
	}
	if p.Token1.Identifier() == "" {
		err := errors.New("pool descriptor: empty token1")
		util.Errorf(err.Error())
		return err
	}
	util.Debugf("pool_descriptor: key=%s validated", p.CompositeKey())
	return nil
}

// normalizeAMM приводит значение amm_version к низкому регистру и добавляет префикс "v".
func normalizeAMM(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "v") {
		trimmed = "v" + trimmed
	}
	return trimmed
}

// normalizeNetwork приводит название сети к нижнему регистру и заменяет разделители.
func normalizeNetwork(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	trimmed = strings.ReplaceAll(trimmed, "-", "_")
	trimmed = strings.ReplaceAll(trimmed, " ", "_")
	return trimmed
}
