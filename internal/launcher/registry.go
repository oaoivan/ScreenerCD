package launcher

import (
	"fmt"
	"strings"

	"github.com/yourusername/screner/internal/config"
)

// Builder описывает функцию, которая регистрирует и запускает коннектор в рамках переданного контекста.
// Возвращаемый список горутин (функций запуска) позволит LaunchAll стартовать всё единообразно.
type Builder func(ctx LaunchContext, exName string, symbols []string, dexCfg *config.DexConfig) error

var registry = map[string]Builder{}

// Register добавляет builder в реестр.
func Register(name string, builder Builder) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		panic("launcher: empty connector name")
	}
	if builder == nil {
		panic(fmt.Sprintf("launcher: nil builder for %s", name))
	}
	if _, exists := registry[key]; exists {
		panic(fmt.Sprintf("launcher: duplicate registration for %s", key))
	}
	registry[key] = builder
}

// Get возвращает зарегистрированный builder или nil.
func Get(name string) Builder {
	return registry[strings.ToLower(strings.TrimSpace(name))]
}

// Keys возвращает список зарегистрированных имён (для диагностики/тестов).
func Keys() []string {
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	return keys
}
