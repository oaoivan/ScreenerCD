# Integration Tests for the Arbitrage Screener Project

This directory contains integration tests for the Arbitrage Screener project. Integration tests are designed to verify that different components of the application work together as expected.

## Structure

- **Test Cases**: Each test case should be organized in a way that clearly defines the purpose of the test, the components involved, and the expected outcomes.
- **Test Framework**: Ensure that the tests utilize a consistent framework for execution and reporting results.

## Running Tests

To run the integration tests, use the following command:

```bash
go test ./tests/integration
```

Make sure that all necessary services (Screener Core and Redis) are running before executing the tests.

## Adding New Tests

When adding new tests, follow these guidelines:

1. **Naming**: Use descriptive names for test files and functions to indicate what functionality is being tested.
2. **Setup and Teardown**: Implement setup and teardown procedures to ensure a clean testing environment.
3. **Assertions**: Use assertions to validate the expected outcomes of the tests.

## Documentation

Refer to the main project README.md for additional information on the overall architecture and setup of the Arbitrage Screener project.