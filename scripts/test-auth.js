const { ClobClient } = require("@polymarket/clob-client");
const { Wallet } = require("ethers");
require("dotenv").config();

async function testAuth() {
    const pk = process.env.POLY_PK;
    const apiKey = process.env.POLY_API_KEY;
    const apiSecret = process.env.POLY_API_SECRET;
    const passphrase = process.env.POLY_PASSPHRASE;

    console.log("=== Node.js Polymarket Auth Test ===");
    console.log("API Key:", apiKey);
    console.log("Secret:", apiSecret);
    console.log("Passphrase:", passphrase?.substring(0, 10) + "...");

    const signer = new Wallet(pk);
    console.log("Wallet:", signer.address);

    // Create client with L2 credentials
    const client = new ClobClient(
        "https://clob.polymarket.com",
        137,
        signer,
        { key: apiKey, secret: apiSecret, passphrase: passphrase }
    );

    try {
        console.log("\nTesting get balance...");
        const balance = await client.getBalanceAllowance({ asset_type: "COLLATERAL" });
        console.log("✅ Balance:", balance);
    } catch (err) {
        console.log("❌ Error:", err.message);
        if (err.response) {
            console.log("Status:", err.response.status);
            console.log("Data:", err.response.data);
        }
    }
}

testAuth();
