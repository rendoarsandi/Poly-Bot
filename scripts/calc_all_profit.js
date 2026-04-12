const https = require('https');

const ADDRESS = '0xe0229e10a858860218b6132f4234602c47bd6603';

const fetchJson = (url) => new Promise((resolve, reject) => {
    https.get(url, (res) => {
        let data = '';
        res.on('data', chunk => data += chunk);
        res.on('end', () => {
            if (res.statusCode >= 400) return reject(new Error(`Status ${res.statusCode}`));
            try {
                resolve(JSON.parse(data));
            } catch (e) {
                reject(e);
            }
        });
    }).on('error', reject);
});

async function main() {
    let allTrades = [];
    let offset = 0;
    
    console.log("Fetching all trades...");
    while (true) {
        try {
            const url = `https://data-api.polymarket.com/trades?user=${ADDRESS}&limit=1000&offset=${offset}&takerOnly=false`;
            const trades = await fetchJson(url);
            if (!Array.isArray(trades) || trades.length === 0) break;
            allTrades = allTrades.concat(trades);
            offset += trades.length;
            console.log(`Fetched ${allTrades.length} trades so far...`);
            if (trades.length < 1000) break;
        } catch (e) {
            console.log("Stopped fetching trades due to error (likely max offset reached):", e.message);
            break;
        }
    }
    
    console.log(`\nFound ${allTrades.length} trades total.`);

    const now = Date.now();
    const ms24h = 24 * 60 * 60 * 1000;
    const ms5h = 5 * 60 * 60 * 1000;

    const markets = {};
    const markets24h = {};
    const markets5h = {};
    
    const applyTrade = (mks, t) => {
        if (!mks[t.slug]) {
            mks[t.slug] = { spent: 0, shares: { "Up": 0, "Down": 0, "Yes": 0, "No": 0 } };
        }
        if (t.side === 'BUY') {
            mks[t.slug].spent += t.size * t.price;
            mks[t.slug].shares[t.outcome] = (mks[t.slug].shares[t.outcome] || 0) + t.size;
        } else if (t.side === 'SELL') {
            mks[t.slug].spent -= t.size * t.price;
            mks[t.slug].shares[t.outcome] = (mks[t.slug].shares[t.outcome] || 0) - t.size;
        }
    };

    allTrades.forEach(t => {
        applyTrade(markets, t);
        const tTime = t.timestamp * 1000; // polymarket timestamp is usually in seconds
        if (now - tTime <= ms24h) applyTrade(markets24h, t);
        if (now - tTime <= ms5h) applyTrade(markets5h, t);
    });

    const calculateProfitForMarkets = async (mks, label) => {
        let totalProfit = 0;
        let resolvedProfit = 0;
        let totalInvested = 0;
        let unresolvedCount = 0;

        for (const slug of Object.keys(mks)) {
            try {
                const eventData = await fetchJson(`https://gamma-api.polymarket.com/events?slug=${slug}`);
                if (!eventData || eventData.length === 0) continue;
                
                const marketInfo = eventData[0].markets.find(m => m.slug === slug) || eventData[0].markets[0];
                const m = mks[slug];
                totalInvested += m.spent;

                let winnings = 0;
                let merged = 0;

                const outcomes = JSON.parse(marketInfo.outcomes || '["Up", "Down"]');
                const prices = JSON.parse(marketInfo.outcomePrices || '["0", "1"]');
                
                if (outcomes.length === 2 && m.shares[outcomes[0]] > 0 && m.shares[outcomes[1]] > 0) {
                    merged = Math.min(m.shares[outcomes[0]], m.shares[outcomes[1]]);
                    winnings += merged;
                    m.shares[outcomes[0]] -= merged;
                    m.shares[outcomes[1]] -= merged;
                }

                if (marketInfo.closed && marketInfo.umaResolutionStatus === "resolved") {
                    outcomes.forEach((outcome, idx) => {
                        if (prices[idx] === "1" || prices[idx] === 1) {
                            winnings += m.shares[outcome];
                        }
                    });
                    const profit = winnings - m.spent;
                    totalProfit += profit;
                    resolvedProfit += profit;
                } else {
                    unresolvedCount++;
                    const unrealizedPnl = winnings - m.spent; 
                    totalProfit += unrealizedPnl;
                }
            } catch (e) {
                // ignore
            }
        }
        console.log(`\n=== ${label} ===`);
        console.log(`Total Invested: $${totalInvested.toFixed(2)}`);
        console.log(`Resolved Profit: $${resolvedProfit.toFixed(2)}`);
        console.log(`Estimated Total Profit: $${totalProfit.toFixed(2)}`);
    };

    console.log(`Processing markets...`);
    await calculateProfitForMarkets(markets, "Total / Overall Growth");
    await calculateProfitForMarkets(markets24h, "Last 24 Hours");
    await calculateProfitForMarkets(markets5h, "Last 5 Hours");
}

main();