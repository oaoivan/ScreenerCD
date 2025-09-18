const socket = new WebSocket('ws://localhost:8080'); // Replace with the actual API Gateway WebSocket URL

socket.onopen = function(event) {
    console.log('WebSocket connection established');
};

socket.onmessage = function(event) {
    const data = new Uint8Array(event.data);
    // Deserialize the Protobuf message here
    // Example: const message = ArbitrageOpportunity.deserializeBinary(data);
    console.log('Received data:', data);
    // Update the UI with the received data
};

socket.onclose = function(event) {
    console.log('WebSocket connection closed:', event);
};

socket.onerror = function(error) {
    console.error('WebSocket error:', error);
};