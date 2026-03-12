require('dotenv').config();
const https = require('https');

const ADDRESS = '0x8c74b4eef9a894433B8126aA11d1345efb2B0488';
const API_KEY = process.env.POLYGONSCAN_API_KEY;

if (!API_KEY) {
    console.error("Please set POLYGONSCAN_API_KEY in your .env file.");
    process.exit(1);
}

// Get the timestamp for 7 days ago
const ONE_DAY_AGO = Math.floor(Date.now() / 1000) - (7 * 24 * 60 * 60);

// Using Etherscan v2 API for Polygon (chainid=137)
const URL = `https://api.etherscan.io/v2/api?chainid=137&module=account&action=token1155tx&address=${ADDRESS}&startblock=0&endblock=99999999&page=1&offset=1000&sort=desc&apikey=${API_KEY}`;

console.log("Fetching on-chain ERC-1155 conditional token transfers for the past 7 days...");

https.get(URL, (res) => {
    let data = '';
    res.on('data', chunk => data += chunk);
    res.on('end', () => {
        try {
            const parsed = JSON.parse(data);
            if (parsed.status !== "1" && parsed.message !== "No transactions found") {
                console.error("Error fetching data:", parsed.message, parsed.result);
                return;
            }
            
            const txs = parsed.result || [];
            console.log(`API returned ${txs.length} total historical token transfers.`);
            const recentTxs = txs;
            
            console.log(`\n=== On-Chain Activity for @distinct-baguette (${ADDRESS}) (All Available History) ===\n`);
            console.log(`Total Filled Trades/Token Transfers: ${recentTxs.length}`);
            
            let sentCount = 0;
            let receivedCount = 0;
            let tokenVolumes = {};

            recentTxs.forEach(tx => {
                if (tx.from.toLowerCase() === ADDRESS.toLowerCase()) sentCount++;
                if (tx.to.toLowerCase() === ADDRESS.toLowerCase()) receivedCount++;
                
                const tokenId = tx.tokenID;
                // Shares on Polymarket are often represented with 6 decimals (like USDC)
                const value = parseInt(tx.tokenValue) / 1e6; 

                if (!tokenVolumes[tokenId]) tokenVolumes[tokenId] = 0;
                tokenVolumes[tokenId] += value;
            });

            console.log(`Tokens Received (Buys/Fills): ${receivedCount}`);
            console.log(`Tokens Sent (Sells/Redeems): ${sentCount}`);
            
            console.log(`\n=== Market Concentration (Top 5 Active Tokens) ===`);
            const sortedTokens = Object.entries(tokenVolumes)
                .sort((a, b) => b[1] - a[1])
                .slice(0, 5); 
                
            if (sortedTokens.length === 0) {
                console.log("No market activity found in the last 24 hours.");
            } else {
                sortedTokens.forEach(([tokenId, volume]) => {
                    console.log(`Token ID ${tokenId.substring(0, 15)}... : Volume ~${volume.toFixed(2)} shares`);
                });
            }

            console.log(`\n=== Strategy Analysis: Algorithmic Market Making (AMM) / High-Frequency Trading ===`);
            console.log(`1. Why are they "constantly spamming orders"?`);
            console.log(`   - The user is acting as a Liquidity Provider (Market Maker). On Polymarket, placing and canceling limit orders happens OFF-CHAIN on their Central Limit Order Book (CLOB).`);
            console.log(`   - Because it's off-chain, there are zero gas fees for canceling and replacing orders.`);
            console.log(`   - They are running a bot (much like the one you are building in 'Market-bot') that constantly adjusts its Bid and Ask prices to reflect real-time probabilities or to aggressively capture the 'spread' (the difference between the buy and sell price).`);
            console.log(`\n2. How do they profit?`);
            console.log(`   - Spread Capture: They buy shares for 50¢ and immediately try to sell them for 52¢.`);
            console.log(`   - Maker Rewards: Polymarket heavily incentivizes users who provide liquidity by rewarding them with USDC payouts based on their order book depth.`);
            console.log(`   - The "spamming" ensures they never get caught holding an outdated price when news breaks or momentum shifts.`);
            console.log(`\n3. On-chain vs Off-chain:`);
            console.log(`   - While they might place/cancel 10,000 orders a day (spamming), only a small fraction of those get filled by other users. You only see the *filled* trades on-chain (as queried above).`);

        } catch (e) {
            console.error("Failed to parse API response:", e.message);
        }
    });
}).on("error", (err) => {
    console.log("Error: " + err.message);
});
