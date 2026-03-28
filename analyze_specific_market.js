const fs = require('fs');

const trades = JSON.parse(fs.readFileSync('new_wallet_trades.json', 'utf8'));

// Identify top 3 most active markets by trade count
const marketCounts = {};
trades.forEach(t => {
    marketCounts[t.slug] = (marketCounts[t.slug] || 0) + 1;
});

const topMarkets = Object.entries(marketCounts)
    .sort((a, b) => b[1] - a[1])
    .slice(0, 3);

console.log("Top 3 Markets analyzed:");
topMarkets.forEach(([slug, count]) => console.log(`- ${slug}: ${count} trades`));

const targetSlug = topMarkets[0][0];
console.log(`\n--- Deep Analysis of Market: ${targetSlug} ---`);

const marketTrades = trades.filter(t => t.slug === targetSlug).sort((a, b) => a.timestamp - b.timestamp);

let upBuys = [], downBuys = [], upSells = [], downSells = [];

marketTrades.forEach(t => {
    const tradeTime = new Date(t.timestamp * 1000).toISOString().split('T')[1];
    const outcome = t.outcome;
    const side = t.side;
    const price = t.price;
    const size = t.size;

    console.log(`[${tradeTime}] ${side} ${size} @ ${price} (${outcome})`);

    if (outcome === 'Up') {
        if (side === 'BUY') upBuys.push(t); else upSells.push(t);
    } else {
        if (side === 'BUY') downBuys.push(t); else downSells.push(t);
    }
});

// Profit analysis for this market
function calcStats(trades) {
    if (trades.length === 0) return { avg: 0, total: 0 };
    const totalSize = trades.reduce((acc, t) => acc + t.size, 0);
    const avgPrice = trades.reduce((acc, t) => acc + (t.size * t.price), 0) / totalSize;
    return { avg: avgPrice.toFixed(4), total: totalSize.toFixed(2) };
}

const upBuyStats = calcStats(upBuys);
const upSellStats = calcStats(upSells);
const downBuyStats = calcStats(downBuys);
const downSellStats = calcStats(downSells);

console.log(`\nSummary for ${targetSlug}:`);
console.log(`Up:   Bought ${upBuyStats.total} @ avg ${upBuyStats.avg} | Sold ${upSellStats.total} @ avg ${upSellStats.avg}`);
console.log(`Down: Bought ${downBuyStats.total} @ avg ${downBuyStats.avg} | Sold ${downSellStats.total} @ avg ${downSellStats.avg}`);

// Check for "Arbitrage" or "Merge" opportunity: Up price + Down price < 1.00
// Note: If they buy both at say 0.49 and 0.49, they spent 0.98 to get $1.00 guaranteed (merging).
// This is a common way to profit with low risk.

console.log("\nStrategy Check:");
if (upBuys.length > 0 && downBuys.length > 0) {
    // Check if they buy both around the same time
    const upBuyTimes = upBuys.map(t => t.timestamp);
    const downBuyTimes = downBuys.map(t => t.timestamp);
    const timeOverlap = upBuyTimes.some(t => downBuyTimes.includes(t) || downBuyTimes.includes(t+1) || downBuyTimes.includes(t-1));
    console.log(`- Buys both outcomes near-simultaneously: ${timeOverlap ? "YES" : "NO"}`);
    
    const combinedAvgBuy = parseFloat(upBuyStats.avg) + parseFloat(downBuyStats.avg);
    console.log(`- Combined Avg Buy Price: ${combinedAvgBuy.toFixed(4)} (Expected < 1.00 for merge/arb)`);
}
