# Arbitrage Screener

## Overview
The Arbitrage Screener is a high-performance monitoring tool designed to identify arbitrage opportunities between centralized exchanges (CEX) and decentralized exchanges (DEX) in real-time. The project leverages various technologies to ensure efficient data processing and user-friendly interaction.

## Project Structure
The project is organized into several key directories:

- **cmd/**: Contains the entry points for the Screener Core and API Gateway services.
- **internal/**: Houses the core logic, including configuration management, exchange connectors, data processing, and Redis interactions.
- **pkg/**: Contains Protobuf definitions and common data models.
- **configs/**: Configuration files for the Screener Core and API Gateway services.
- **web/**: Frontend files, including HTML, CSS, and JavaScript for user interaction.
- **scripts/**: Utility scripts for generating Protobuf code.
- **tests/**: Integration tests documentation.
- **Dockerfiles**: Configuration for building Docker images for the services.

## Technologies Used
- **Programming Language**: Go (for backend services)
- **Database/Cache**: Redis
- **Data Serialization**: Protocol Buffers
- **Web Server/Proxy**: Nginx (for production deployment)
- **Containerization**: Docker, Docker Compose
- **Frontend**: HTML, CSS, JavaScript (with protobuf.js)

## Getting Started
To set up the project, follow these steps:

1. **Clone the repository**:
   ```
   git clone <repository-url>
   cd screner
   ```

2. **Install dependencies**:
   Ensure you have Go and Docker installed on your machine.

3. **Build the services**:
   Use the provided Dockerfiles to build the images for the Screener Core and API Gateway.

4. **Run the application**:
   Use Docker Compose to start all services:
   ```
   docker-compose up
   ```

5. **Access the frontend**:
   Open your web browser and navigate to `http://localhost:8080` to interact with the application.

## Быстрый старт (единые скрипты)

В корне добавлены утилиты для запуска/остановки и статуса:

- `scripts/start_all.sh`
   - По умолчанию: запускает локальный `screener-core` (go build) и поднимает Redis через Docker Compose при необходимости.
   - Ключи:
      - `--docker-all` — запустить весь стек в Docker (`redis + screener-core`, с `--with-api` — ещё и `api-gateway`).
      - `--with-api` — вместе с `--docker-all` поднимет `api-gateway`.
      - `--no-build` — не собирать бинарник, использовать существующий `build/screener-core`.
      - `--clean-log` — обнулить `screner.log` перед стартом.
   - Переменные окружения: `REDIS_HOST` (default `localhost`), `REDIS_PORT` (default `6379`).

- `scripts/stop_all.sh`
   - Останавливает локальный `screener-core` по PID.
   - Ключ `--docker-all` (и опционально `--with-api`) — остановит docker compose сервисы.

- `scripts/status_all.sh`
   - Показывает состояние docker compose, PING Redis + счётчики ключей, локальный PID, последние строки логов.

Логи приложения: `screner.log`. PID локального процесса: `build/screener-core.pid`.
## Contributing
Contributions are welcome! Please submit a pull request or open an issue for any enhancements or bug fixes.

## License
This project is licensed under the MIT License. See the LICENSE file for details.