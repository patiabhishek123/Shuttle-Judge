const { CommClient } = require('caspian-sdk');
require('dotenv').config({ path: '../.env' });

const client = new CommClient();

async function run() {
    try {
        console.log("Caspian Base URL:", process.env.CASPIAN_BASE_URL);
        console.log("Caspian API Key:", process.env.CASPIAN_API_KEY ? "Loaded" : "Missing");
        
        const channels = await client.channels();
        console.log("\n--- Available Channels ---");
        console.log(JSON.stringify(channels, null, 2));

        const billing = await client.billing();
        console.log("\n--- Billing Info ---");
        console.log(JSON.stringify(billing, null, 2));
    } catch (err) {
        console.error("Failed to query Caspian API:", err);
    }
}

run();
