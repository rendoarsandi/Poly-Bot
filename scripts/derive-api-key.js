#!/usr/bin/env node
/**
 * Polymarket API Key Derivation Script
 *
 * This script derives your API credentials from your private key.
 * You only need to run this once - save the output to your .env file.
 *
 * Usage:
 *   node scripts/derive-api-key.js
 *
 * Requirements:
 *   - Set POLY_PK environment variable with your private key
 *   - Or pass it as an argument: node scripts/derive-api-key.js 0x...
 */

const { Chain, ClobClient } = require("@polymarket/clob-client-v2");
const { createWalletClient, http } = require("viem");
const { privateKeyToAccount } = require("viem/accounts");

const HOST = "https://clob.polymarket.com";
const CHAIN_ID = Chain.POLYGON;

async function deriveApiKey() {
    // Get private key from env or argument
    let privateKey = process.env.POLY_PK || process.argv[2];

    if (!privateKey) {
        console.error("Error: No private key provided");
        console.error("");
        console.error("Usage:");
        console.error("  POLY_PK=0x... node scripts/derive-api-key.js");
        console.error("  node scripts/derive-api-key.js 0x...");
        console.error("");
        console.error("Make sure your private key starts with '0x'");
        process.exit(1);
    }

    // Ensure 0x prefix
    if (!privateKey.startsWith("0x")) {
        privateKey = "0x" + privateKey;
    }

    try {
        console.log("Connecting to Polymarket CLOB...");

        const account = privateKeyToAccount(privateKey);
        const signer = createWalletClient({
            account,
            transport: http(process.env.POLYGON_RPC_URL || "https://polygon-rpc.com"),
        });
        const client = new ClobClient({ host: HOST, chain: CHAIN_ID, signer });

        console.log("Wallet address:", account.address);
        console.log("");
        console.log("Deriving API credentials...");

        const creds = await client.createOrDeriveApiKey();

        console.log("");
        console.log("=".repeat(60));
        console.log("SUCCESS! Add these to your .env file:");
        console.log("=".repeat(60));
        console.log("");
        console.log(`POLY_API_KEY=${creds.key}`);
        console.log(`POLY_API_SECRET=${creds.secret}`);
        console.log(`POLY_PASSPHRASE=${creds.passphrase}`);
        console.log("");
        console.log("=".repeat(60));
        console.log("");
        console.log("Your full .env should include:");
        console.log("  POLY_PK=0x...           (your private key)");
        console.log("  POLY_API_KEY=...        (from above)");
        console.log("  POLY_API_SECRET=...     (from above)");
        console.log("  POLY_PASSPHRASE=...     (from above)");

    } catch (error) {
        console.error("Error deriving API key:", error.message);

        if (error.message.includes("invalid")) {
            console.error("");
            console.error("Check that your private key is valid and properly formatted.");
        }

        process.exit(1);
    }
}

deriveApiKey();
