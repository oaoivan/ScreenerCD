package util

import (
	"encoding/json"
	"io/ioutil"
	"sort"
	"strings"
)

// CoinSymbol представляет информацию о криптовалютном символе
type CoinSymbol struct {
	Symbol      string
	BybitSymbol string
	IsValid     bool
}

// LoadSymbolsFromJSON загружает символы из JSON файла контрактов
func LoadSymbolsFromJSON(filePath string) ([]CoinSymbol, error) {
	// Читаем JSON файл
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	// Парсим JSON
	var jsonData map[string]interface{}
	err = json.Unmarshal(data, &jsonData)
	if err != nil {
		return nil, err
	}

	// Извлекаем все ключи верхнего уровня (тикеры)
	var symbols []CoinSymbol
	for ticker := range jsonData {
		// Пропускаем стейблкоины и fiat валюты
		if isStablecoinOrFiat(ticker) {
			continue
		}

		coinSymbol := CoinSymbol{
			Symbol:      ticker,
			BybitSymbol: ticker + "USDT",
			IsValid:     false, // Пока не проверен
		}
		symbols = append(symbols, coinSymbol)
	}

	// Сортируем по символу для удобства
	sort.Slice(symbols, func(i, j int) bool {
		return symbols[i].Symbol < symbols[j].Symbol
	})

	return symbols, nil
}

// isStablecoinOrFiat проверяет, является ли символ стейблкоином или фиатной валютой
func isStablecoinOrFiat(symbol string) bool {
	stablecoins := []string{
		"USDT", "USDC", "DAI", "BUSD", "USD", "EUR", "GBP", "JPY",
		"USDE", "USD1", "USTC", // добавляем другие стейблкоины из списка
	}

	for _, stable := range stablecoins {
		if strings.EqualFold(symbol, stable) {
			return true
		}
	}
	return false
}

// GetValidBybitSymbols возвращает список символов в формате Bybit
func GetValidBybitSymbols(symbols []CoinSymbol) []string {
	var result []string
	for _, symbol := range symbols {
		if symbol.IsValid {
			result = append(result, symbol.BybitSymbol)
		}
	}
	return result
}

// FilterBybitSymbols фильтрует символы по заданному списку доступных на Bybit
// Это функция-заглушка, позже можно будет добавить API-запрос к Bybit для проверки
func FilterBybitSymbols(symbols []CoinSymbol, availableSymbols []string) []CoinSymbol {
	availableMap := make(map[string]bool)
	for _, symbol := range availableSymbols {
		availableMap[strings.ToUpper(symbol)] = true
	}

	for i := range symbols {
		symbols[i].IsValid = availableMap[strings.ToUpper(symbols[i].BybitSymbol)]
	}

	return symbols
}
