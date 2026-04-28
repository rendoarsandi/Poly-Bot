require('dotenv').config();
const { Chain, ClobClient } = require('@polymarket/clob-client-v2');
const { createWalletClient, http } = require('viem');
const { privateKeyToAccount } = require('viem/accounts');

// Monkey patch WebSocket to see what it sends
const WebSocket = require('ws');
const origSend = WebSocket.prototype.send;
WebSocket.prototype.send = function(data) {
    console.log('Sending WS message:', data.toString());
    origSend.call(this, data);
};

async function main() {
    const account = privateKeyToAccount(process.env.POLY_PK);
    const wallet = createWalletClient({
        account,
        transport: http(process.env.POLYGON_RPC_URL || "https://polygon-rpc.com"),
    });

    const clobClient = new ClobClient({
        host: "https://clob.polymarket.com",
        chain: Chain.POLYGON,
        signer: wallet,
        creds: {
            key: process.env.POLY_API_KEY,
            secret: process.env.POLY_API_SECRET,
            passphrase: process.env.POLY_PASSPHRASE
        }
    });

    console.log("Creating WS Client...");
    // Wait, let's look at the method name or check if there's an export?
}

main().catch(console.error);
