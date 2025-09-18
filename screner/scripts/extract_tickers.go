package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"sort"
)

func main() {
	// Читаем JSON файл
	data, err := ioutil.ReadFile("../../Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		log.Fatalf("Ошибка чтения файла: %v", err)
	}

	// Парсим JSON
	var jsonData map[string]interface{}
	err = json.Unmarshal(data, &jsonData)
	if err != nil {
		log.Fatalf("Ошибка парсинга JSON: %v", err)
	}

	// Извлекаем все ключи верхнего уровня (тикеры)
	var tickers []string
	for ticker := range jsonData {
		tickers = append(tickers, ticker)
	}

	// Сортируем для удобства
	sort.Strings(tickers)

	fmt.Printf("Найдено %d тикеров:\n", len(tickers))
	for i, ticker := range tickers {
		fmt.Printf("%d. %s\n", i+1, ticker)
	}

	// Конвертируем в формат Bybit (добавляем USDT)
	fmt.Println("\nВ формате Bybit (с USDT):")
	var bybitSymbols []string
	for _, ticker := range tickers {
		// Пропускаем некоторые стейблкоины и fiat валюты
		if ticker == "USDT" || ticker == "USDC" || ticker == "DAI" ||
			ticker == "BUSD" || ticker == "USD" || ticker == "EUR" {
			continue
		}
		bybitSymbol := ticker + "USDT"
		bybitSymbols = append(bybitSymbols, bybitSymbol)
		fmt.Printf("%s -> %s\n", ticker, bybitSymbol)
	}

	fmt.Printf("\nИтого для подписки на Bybit: %d символов\n", len(bybitSymbols))

	// Выводим список для копирования в код
	fmt.Println("\nСписок для использования в коде:")
	fmt.Println("var symbols = []string{")
	for i, symbol := range bybitSymbols {
		if i < len(bybitSymbols)-1 {
			fmt.Printf("    \"%s\",\n", symbol)
		} else {
			fmt.Printf("    \"%s\"\n", symbol)
		}
	}
	fmt.Println("}")
}
