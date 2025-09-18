package util

import (
	"encoding/json"
	"io/ioutil"
	"sort"
)

// LoadSymbolsFromFile загружает символы из JSON файла и конвертирует их в формат Bybit
func LoadSymbolsFromFile(filePath string) ([]string, error) {
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
	var tickers []string
	for ticker := range jsonData {
		tickers = append(tickers, ticker)
	}

	// Сортируем для удобства
	sort.Strings(tickers)

	// Конвертируем в формат Bybit (добавляем USDT)
	var bybitSymbols []string
	for _, ticker := range tickers {
		// Пропускаем некоторые стейблкоины и fiat валюты
		if ticker == "USDT" || ticker == "USDC" || ticker == "DAI" ||
			ticker == "BUSD" || ticker == "USD" || ticker == "EUR" {
			continue
		}
		bybitSymbol := ticker + "USDT"
		bybitSymbols = append(bybitSymbols, bybitSymbol)
	}

	return bybitSymbols, nil
}
