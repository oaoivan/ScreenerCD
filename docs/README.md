 # Arbitrage Screener

## Overview
The Arbitrage Screener is a high-performance monitoring tool designed to identify arbitrage opportunities between centralized exchanges (CEX) and decentralized exchanges (DEX) in real-time. The project leverages various technologies to ensure efficient data processing and user-friendly interaction.

## Project Structure
The project is organized into several key directories:

- **cmd/**: Contains the entry point for the Screener Core service.
- **internal/**: Houses the core logic, including configuration management, exchange connectors, data processing, and Redis interactions.
- **pkg/**: Contains Protobuf definitions and common data models.
- **configs/**: Configuration files for the Screener Core service.
- **web/**: Legacy web prototype assets (HTML, CSS, JavaScript).
- **scripts/**: Utility scripts for generating Protobuf code.
- **tests/**: Integration tests documentation.
- **Dockerfile.screener-core**: Container build definition for the Screener Core service.

## Technologies Used
- **Programming Language**: Go (for backend services)
- **Database/Cache**: Redis
- **Data Serialization**: Protocol Buffers
- **Containerization**: Docker, Docker Compose
- **Frontend (prototype)**: HTML, CSS, JavaScript (with protobuf.js)

## Architecture

```mermaid
flowchart LR
   Config[screener-core.yaml\n(YAML config)] --> Resolver[Symbol resolver\ninline → symbols_file → default]
   Resolver --> Bybit[Bybit connector]
   Resolver --> Gate[Gate connector]
   Resolver --> Bitget[Bitget connector]
   Resolver --> OKX[OKX connector]

   Bybit --> Channel[dataChannel\nshared buffer]
   Gate --> Channel
   Bitget --> Channel
   OKX --> Channel

   Channel --> Workers[Redis worker pool\nbatch HSET]
   Workers --> Redis[(Redis)]
   Redis --> Desktop[Desktop client\nreads price:* keys]

   Workers --> Logs[Logs & metrics\napp.log]
```

Symbol lists are defined per exchange in the YAML config: each entry can provide inline pairs or a `symbols_file`, and falls back to the global `default_symbols_file` when unspecified. Для всех коннекторов стартует единая фабрика `internal/launcher`: достаточно зарегистрировать builder под нужным именем и прописать настройки в YAML — `main.go` автоматически подберёт его по ключу. Для DEX-коннекторов дополнительно требуется рабочий `http_url`, чтобы перед подпиской через RPC проверить `token0/token1` и корректно привести цены к USD.

## Getting Started
To set up the project, follow these steps:

1. **Clone the repository**:
   ```
   git clone <repository-url>
   cd screner
   ```

2. **Install dependencies**:
   Ensure you have Go and Docker installed on your machine.

3. **Build the service**:
   Use the provided Dockerfile to build the image for the Screener Core.

4. **Run the application**:
   Use Docker Compose to start the Screener Core together with Redis:
   ```
   docker-compose up
   ```

5. **Использование данных**:
   Screener Core публикует маркет-данные в Redis. Настраиваемый десктопный клиент может читать эти ключи напрямую.

## Быстрый старт (единые скрипты)

В корне добавлены утилиты для запуска/остановки и статуса:

- `scripts/start_all.sh`
   - По умолчанию: запускает локальный `screener-core` (go build) и поднимает Redis через Docker Compose при необходимости.
   - Ключи:
      - `--docker-all` — запустить весь стек в Docker (`redis + screener-core`).
      - `--no-build` — не собирать бинарник, использовать существующий `build/screener-core`.
      - `--clean-log` — обнулить `screner.log` перед стартом.
   - Переменные окружения: `REDIS_HOST` (default `localhost`), `REDIS_PORT` (default `6379`).

- `scripts/stop_all.sh`
   - Останавливает локальный `screener-core` по PID.
   - Ключ `--docker-all` — остановит docker compose сервисы.

- `scripts/status_all.sh`
   - Показывает состояние docker compose, PING Redis + счётчики ключей, локальный PID, последние строки логов.

Логи приложения: `screner.log`. PID локального процесса: `build/screener-core.pid`.
## Contributing
Contributions are welcome! Please submit a pull request or open an issue for any enhancements or bug fixes.

## License
This project is licensed under the MIT License. See the LICENSE file for details.
