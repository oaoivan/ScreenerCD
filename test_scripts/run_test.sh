#!/bin/bash

echo "=== ТЕСТИРОВАНИЕ ВСЕХ ТИКЕРОВ НА BYBIT ==="

cd /home/ivan/Work/Screner

echo "📂 Текущая директория: $(pwd)"
echo "📁 Проверяем файлы:"
ls -la cmd/screener-core/main.go
echo ""

echo "🔍 Проверяем JSON файл с символами:"
ls -la Temp/all_contracts_merged_reformatted.json
echo ""

echo "🚀 Запускаем приложение..."
go run ./cmd/screener-core/main.go