const https = require('https');

const ADDRESS = '0xe0229e10a858860218b6132f4234602c47bd6603';
// Try passing active=false, active=all to see if we get more positions
const URL = `https://data-api.polymarket.com/positions?user=${ADDRESS}&active=false&limit=1000`;

https.get(URL, (res) => {
    let data = '';
    res.on('data', chunk => data += chunk);
    res.on('end', () => {
        try {
            const positions = JSON.parse(data);
            console.log(`Total Positions Found (active=false): ${positions.length}`);
            if (positions.length > 2) {
                let totalCashPnl = 0;
                positions.forEach(p => totalCashPnl += p.cashPnl);
                console.log(`Total Cash PnL: ${totalCashPnl.toFixed(2)}`);
            }
        } catch (e) {}
    });
});

const URL2 = `https://data-api.polymarket.com/positions?user=${ADDRESS}&limit=1000`;
https.get(URL2, (res) => {
    let data = '';
    res.on('data', chunk => data += chunk);
    res.on('end', () => {
        try {
            const positions = JSON.parse(data);
            console.log(`Total Positions Found (default): ${positions.length}`);
        } catch (e) {}
    });
});
