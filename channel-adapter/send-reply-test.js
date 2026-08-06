const { CommClient } = require('caspian-sdk');
require('dotenv').config({ path: '../.env' });

const client = new CommClient({
    apiKey: process.env.CASPIAN_API_KEY,
    baseUrl: process.env.CASPIAN_BASE_URL
});

async function run() {
    try {
        console.log("Triggering reply email...");
        const res = await client.request("POST", "/v1/test-emails", {
            json: {
                connection_id: "conn_d6363af08484aac98d2c5d53",
                text: "Yes, that is correct.",
                subject: "March Electricity Bill Dispute",
                from: "party_a@example.com"
            }
        });
        console.log("Reply email trigger result:", JSON.stringify(res, null, 2));
    } catch (e) {
        console.error("Error:", e.message);
    }
}
run();
