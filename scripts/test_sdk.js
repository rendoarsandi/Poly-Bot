require('dotenv').config();
const { Chain, ClobClient } = require('@polymarket/clob-client-v2');
const { createWalletClient, http } = require('viem');
const { privateKeyToAccount } = require('viem/accounts');

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

    // Let's manually trigger GetPositions using their SDK which doesn't seem to export WS logic?
    // Wait, their github readme says there is a WS client. How is it instantiated?
    // They have createWsClient but wait it wasn't in Object.getOwnPropertyNames ?
    // Let me search their dist folder
}

main().catch(console.error);
