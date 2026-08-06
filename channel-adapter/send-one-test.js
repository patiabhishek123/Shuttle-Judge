const { CommClient } = require('caspian-sdk');
require('dotenv').config({ path: '../.env' });

const client = new CommClient({
    apiKey: process.env.CASPIAN_API_KEY,
    baseUrl: process.env.CASPIAN_BASE_URL
});

async function run() {
    try {
        console.log("Triggering test email...");
        const res = await client.request("POST", "/v1/test-emails", {
            json: {
                connection_id: "conn_d6363af08484aac98d2c5d53",
                text: "Hello, I want to open a case about our electricity bill from March. I paid $340 but the other party refuses to pay their half.",
                subject: "March Electricity Bill Dispute",
                from: "party_a@example.com"
            }
        });
        console.log("Test email trigger result:", JSON.stringify(res, null, 2));
    } catch (e) {
        console.error("Error:", e.message);
    }
}
run();
