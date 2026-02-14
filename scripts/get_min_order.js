const axios = require('axios');

async function getMinOrderSize(tokenId) {
    try {
        const response = await axios.get(`https://clob.polymarket.com/book?token_id=${tokenId}`);
        const data = response.data;
        
        console.log(`Token ID: ${tokenId}`);
        console.log(`Minimum Order Size: ${data.min_order_size}`);
        console.log(`Tick Size: ${data.tick_size}`);
    } catch (error) {
        console.error('Error fetching order book:', error.message);
        if (error.response) {
            console.error('Response data:', error.response.data);
        }
    }
}

const tokenId = process.argv[2];
if (!tokenId) {
    console.error('Please provide a token_id as an argument.');
    process.exit(1);
}

getMinOrderSize(tokenId);
