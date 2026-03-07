require('dotenv').config();
const { ClobClient } = require('@polymarket/clob-client');
const { ethers } = require('ethers');

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

    // Let's manually trigger GetPositions using their SDK which doesn't seem to export WS logic?
    // Wait, their github readme says there is a WS client. How is it instantiated?
    // They have createWsClient but wait it wasn't in Object.getOwnPropertyNames ?
    // Let me search their dist folder
}

main().catch(console.error);
