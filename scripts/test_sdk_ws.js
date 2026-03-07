require('dotenv').config();
const { ClobClient } = require('@polymarket/clob-client');
const { ethers } = require('ethers');

// Monkey patch WebSocket to see what it sends
const WebSocket = require('ws');
const origSend = WebSocket.prototype.send;
WebSocket.prototype.send = function(data) {
    console.log('Sending WS message:', data.toString());
    origSend.call(this, data);
};

async function main() {
    const provider = new ethers.providers.JsonRpcProvider(process.env.POLYGON_RPC_URL || "https://polygon-rpc.com");
    const wallet = new ethers.Wallet(process.env.POLY_PK, provider);
    
    // Hardcode poly chain id for prod
    const chainId = 137;

    const clobClient = new ClobClient(
        "https://clob.polymarket.com",
        chainId,
        wallet,
        {
            key: process.env.POLY_API_KEY,
            secret: process.env.POLY_API_SECRET,
            passphrase: process.env.POLY_PASSPHRASE
        }
    );

    console.log("Creating WS Client...");
    // Wait, let's look at the method name or check if there's an export?
}

main().catch(console.error);
