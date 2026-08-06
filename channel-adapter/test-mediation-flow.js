const axios = require('axios');

const BACKEND_URL = 'http://localhost:8080';

async function sendMsg(convId, sender, text) {
    console.log(`\n[${sender}] -> "${text}"`);
    const resp = await axios.post(`${BACKEND_URL}/message`, {
        conversation_id: convId,
        channel: 'email',
        text: text,
        sender_ref: sender
    });
    console.log(`[Docket] -> "${resp.data.reply_text.replace(/\n/g, ' ')}"`);
    return resp.data.reply_text;
}

async function run() {
    try {
        console.log("=== STARTING SHUTTLE COURT MEDIATION FLOW TEST ===");

        // 1. Party A opens the case
        let reply = await sendMsg(
            'conv_party_a', 
            'party_a@example.com', 
            'I want to open a case about our electricity bill from March. I paid $340 but the other party refuses to pay their half.'
        );

        // Extract join code
        const joinCodeMatch = reply.match(/join code with the other party: \*\*([A-Z0-9]+)\*\*/i);
        if (!joinCodeMatch) {
            throw new Error("Failed to extract join code from welcome message");
        }
        const joinCode = joinCodeMatch[1];
        console.log(`>>> Extracted Join Code: ${joinCode}`);

        // 2. Party A continues intake (provides clarifications)
        await sendMsg(
            'conv_party_a',
            'party_a@example.com',
            'I paid $340 on March 15 for electricity. I want them to pay half ($170).'
        );

        // 3. Party A confirms restatement
        await sendMsg(
            'conv_party_a',
            'party_a@example.com',
            'Yes, that is correct.'
        );

        // 4. Party B joins using the join code
        await sendMsg(
            'conv_party_b',
            'party_b@example.com',
            joinCode
        );

        // 5. Party B provides their side (deliberate contradiction: $300 instead of $340)
        await sendMsg(
            'conv_party_b',
            'party_b@example.com',
            'I paid $150 for March electricity. The bill was actually $300 total.'
        );

        // 6. Party B confirms restatement
        await sendMsg(
            'conv_party_b',
            'party_b@example.com',
            'Yes, that is correct.'
        );

        // 7. Party B responds to the cross-check question (resolving the contradiction)
        await sendMsg(
            'conv_party_b',
            'party_b@example.com',
            'Ah, sorry, looking at the bill statement again, it was indeed $340. I was looking at the February one.'
        );

        // 8. Party A votes YES on the proposal
        await sendMsg(
            'conv_party_a',
            'party_a@example.com',
            'YES'
        );

        // 9. Party B votes YES on the proposal
        await sendMsg(
            'conv_party_b',
            'party_b@example.com',
            'YES'
        );

        console.log("\n=== FLOW TEST COMPLETED SUCCESSFULLY ===");
    } catch (e) {
        console.error("Test failed:", e.message);
        if (e.response && e.response.data) {
            console.error("Backend error details:", e.response.data);
        }
    }
}

run();
