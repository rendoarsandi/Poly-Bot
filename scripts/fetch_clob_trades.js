require('dotenv').config();
const { ClobClient } = require('@polymarket/clob-client');
const { ethers } = require('ethers');

async function main() {
    console.log("Authenticating with Polymarket CLOB...");
    
    // We don't actually need a real wallet just to read data, but the SDK might require one
    const provider = new ethers.providers.JsonRpcProvider(process.env.POLYGON_RPC_URL || "https://polygon-rpc.com");
    const wallet = new ethers.Wallet(process.env.POLY_PK, provider);
    
    const clobClient = new ClobClient(
        "https://clob.polymarket.com",
        137,
        wallet,
        {
            key: process.env.POLY_API_KEY,
            secret: process.env.POLY_API_SECRET,
            passphrase: process.env.POLY_PASSPHRASE
        }
    );

    const targetAddress = "0xe00740bce98a594e26861838885ab310ec3b548c";
    console.log(`Fetching recent trades for ${targetAddress}...`);

    try {
        // The SDK has a getTrades method
        // getTrades(params: { maker_address?: string, token_id?: string })
        const trades = await clobClient.getTrades({ maker_address: targetAddress });
        
        if (!trades || trades.length === 0) {
            console.log("No recent trades found on the CLOB for this address.");
        } else {
            console.log(`Found ${trades.length} recent trades.`);
            trades.slice(0, 10).forEach((t, i) => {
                console.log(`Trade ${i+1}: Price $${t.price}, Size ${t.size}, Side: ${t.side}`);
            });
            
            // Check for spam behavior
            const sizes = trades.map(t => parseFloat(t.size));
            const avgSize = sizes.reduce((a, b) => a + b, 0) / sizes.length;
            console.log(`\nAverage Trade Size: ${avgSize.toFixed(2)} shares`);
        }
    } catch (e) {
        console.error("Failed to fetch CLOB trades:", e.message);
    }
}

main().catch(console.error);
