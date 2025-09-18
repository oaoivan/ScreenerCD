package main

import (
	"fmt"
	"log"

	"github.com/yourusername/screner/internal/util"
)

func main() {
	// Загружаем символы из JSON файла
	symbols, err := util.LoadSymbolsFromJSON("Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		log.Fatalf("Ошибка загрузки символов: %v", err)
	}

	fmt.Printf("Загружено %d символов из JSON файла:\n", len(symbols))

	// Выводим первые 20 символов для проверки
	fmt.Println("\nПервые 20 символов:")
	for i, symbol := range symbols {
		if i >= 20 {
			break
		}
		fmt.Printf("%d. %s -> %s\n", i+1, symbol.Symbol, symbol.BybitSymbol)
	}

	// Для начала возьмем популярные символы для тестирования
	testSymbols := []string{
		"BTCUSDT", "ETHUSDT", "LTCUSDT", "ADAUSDT", "DOTUSDT",
		"LINKUSDT", "XRPUSDT", "BCHUSDT", "BNBUSDT", "SOLUSDT",
		"AVAXUSDT", "MATICUSDT", "ATOMUSDT", "NEARUSDT", "ALGOUSDT",
		"FILUSDT", "ICPUSDT", "FTMUSDT", "SANDUSDT", "MANAUSDT",
	}

	// Фильтруем символы по доступным для тестирования
	filteredSymbols := util.FilterBybitSymbols(symbols, testSymbols)

	// Получаем список валидных символов
	validSymbols := util.GetValidBybitSymbols(filteredSymbols)

	fmt.Printf("\nВалидных символов для тестирования: %d\n", len(validSymbols))
	fmt.Println("Символы для подписки:")
	for i, symbol := range validSymbols {
		fmt.Printf("%d. %s\n", i+1, symbol)
	}

	// Создаем список символов для использования в main.go
	fmt.Println("\nГенерируем код для main.go:")
	fmt.Println("var symbols = []string{")
	for i, symbol := range validSymbols {
		if i < len(validSymbols)-1 {
			fmt.Printf("    \"%s\",\n", symbol)
		} else {
			fmt.Printf("    \"%s\"\n", symbol)
		}
	}
	fmt.Println("}")
}
