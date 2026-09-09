import test from "node:test";
import assert from "node:assert/strict";
import { createBot } from "../src/bot.js";
import { isRandomMode, isRealMode } from "../src/real-number.js";

const botInfo = { id: 999, is_bot: true, first_name: "Test", username: "test_bot", can_join_groups: true, can_read_all_group_messages: false, supports_inline_queries: false };

function mockDb() {
  const users = new Map();
  const numbers = new Map();
  let numberSeq = 1;
  return {
    _userCache: new Map(),
    _cachedUser: null,
    upsertUser: async (from, chatID, language = "ru") => {
      if (!users.has(from.id)) {
        users.set(from.id, { telegram_id: from.id, chat_id: chatID, username: from.username ?? "", first_name: from.first_name ?? "", server_user_id: 0, language, notifications: 1, bonus: 0, referred_by: null, referral_count: 0, daily_day: "", spin_day: "", spin_day_count: 0, spin_week: "", spin_week_count: 0, created_at: 0, updated_at: 0 });
      }
      return users.get(from.id);
    },
    user: async (id) => users.get(id) ?? null,
    userByChatID: async (chatID) => { for (const u of users.values()) if (u.chat_id === chatID) return u; return null; },
    users: async () => [...users.values()],
    notificationRecipients: async () => [...users.values()].filter((u) => u.notifications === 1),
    stats: async () => ({ users: users.size, numbers: numbers.size, sales: 0 }),
    setLanguage: async () => {},
    toggleNotifications: async (id) => { const u = users.get(id); if (u) { u.notifications = u.notifications ? 0 : 1; return Boolean(u.notifications); } return false; },
    setServerUserID: async (id, sid) => { const u = users.get(id); if (u) u.server_user_id = sid; },
    addBonus: async (id, amount) => { const u = users.get(id); if (!u) throw new Error("invalid Telegram ID"); u.bonus = Math.max(0, u.bonus + amount); return u.bonus; },
    claimDaily: async (id, amount) => ({ claimed: true, balance: amount }),
    createNumber: async (ownerID, chatID, format = "free", country = "RU", replace = false) => {
      const existing = [...numbers.values()].find((n) => n.owner_id === ownerID && n.is_current);
      if (existing && !replace) return existing;
      if (replace && existing) existing.is_current = false;
      const id = numberSeq++;
      const phone = `+7999${String(id).padStart(7, "0")}`;
      const num = { id, phone, display: phone, format, country, owner_id: ownerID, chat_id: chatID, is_current: true, login_code: "12345", code_expires_at: 9999999999, created_at: 0 };
      numbers.set(id, num);
      return num;
    },
    currentNumber: async (ownerID) => [...numbers.values()].find((n) => n.owner_id === ownerID && n.is_current) ?? null,
    numbers: async (ownerID) => [...numbers.values()].filter((n) => n.owner_id === ownerID),
    findNumber: async (phone) => [...numbers.values()].find((n) => n.phone === phone) ?? null,
    updateLoginCode: async (phone, code) => { const n = [...numbers.values()].find((x) => x.phone === phone); if (n) { n.login_code = code; return { number: n, chatIDs: [n.chat_id] }; } return { number: null, chatIDs: [] }; },
    acceptLoginCodeDelivery: async () => ({ duplicate: false, number: null, chatIDs: [] }),
    grantCodeAccess: async () => {},
    revokePurchasedNumber: async () => false,
    getSetting: async () => "20",
    setSetting: async () => {},
    starsRate: async () => 20,
    setPending: async () => {},
    pending: async () => null,
    clearPending: async () => {},
    recentRecipients: async () => [],
    rememberRecipient: async () => {},
    reserveSpin: async () => ({ prize: 50, day: "2026-01-01" }),
    finishSpin: async () => {},
    createPromo: async (code) => code,
    claimPromo: async (code) => ({ stars_amount: 50 }),
    createGiveaway: async () => ({ id: "abc", text: "test", stars_amount: 10 }),
    claimGiveaway: async () => ({ stars_amount: 10 }),
    releaseCampaignClaim: async () => {},
    beginPayment: async () => true,
    finishPayment: async () => {},
    failPayment: async () => {},
    addSale: async () => {},
    saleByCharge: async () => null,
    recentSales: async () => [],
    refundByCharge: async () => null,
    isRefunded: async () => false,
    beginRefund: async () => ({ internal_reversed: false }),
    markRefundInternal: async () => {},
    failRefund: async () => {},
    markRefunded: async () => {},
    addSupportMessage: async () => 1,
    supportMessage: async () => null,
    closeSupportMessage: async () => {},
    verifiedPhone: async (id) => {
      for (const u of users.values()) if (u.telegram_id === id && u._verifiedPhone) return { phone: u._verifiedPhone, telegram_id: id, chat_id: u.chat_id };
      return null;
    },
    bindVerifiedPhone: async (id, chatID, phone) => {
      const u = users.get(id); if (u) u._verifiedPhone = phone;
      return { phone, telegram_id: id, chat_id: chatID };
    },
    unbindVerifiedPhone: async (id) => { const u = users.get(id); if (u) delete u._verifiedPhone; return true; },
    adminLookupByNumber: async () => null,
    adminLookupByTelegramID: async () => null,
    close: async () => {},
  };
}

function createBotWithMode(mode) {
  const db = mockDb();
  const config = {
    botToken: "999:TEST", defaultLanguage: "ru", defaultNumberCountry: "RU", botMode: mode,
    ownerIDs: new Set(), requiredChannel: "", requiredChannelURL: "", referralBonus: 100,
    dailyBonus: 10, notificationTTLDays: 30, productName: "Telesrv", publicUsername: "test_bot",
    gramsrvAPI: "http://localhost:9999", gramsrvToken: "test", gramsrvActor: "test",
    publicBaseURL: "https://example.com", codeHost: "127.0.0.1", codePort: 0,
    codeWebhookSecret: "test-secret-12345678901234",
  };
  const calls = [];
  const bot = createBot({ config, db, gramsrv: {} });
  bot.botInfo = botInfo;
  bot.api.config.use(async (_prev, method, payload) => {
    calls.push({ method, payload });
    if (method === "answerCallbackQuery") return { ok: true, result: true };
    return { ok: true, result: { message_id: 100, date: 1, chat: { id: payload.chat_id ?? 1, type: "private" }, text: payload.text ?? "" } };
  });
  return { bot, calls, db, config };
}

test("isRandomMode and isRealMode correctly identify mode", () => {
  assert.equal(isRandomMode({ botMode: "random" }), true);
  assert.equal(isRandomMode({ botMode: "real" }), false);
  assert.equal(isRealMode({ botMode: "real" }), true);
  assert.equal(isRealMode({ botMode: "random" }), false);
});

test("random mode allows number generation", async () => {
  const { bot, calls, db } = createBotWithMode("random");
  await db.upsertUser({ id: 10, first_name: "User" }, 10, "ru");
  await bot.handleUpdate({
    update_id: 1,
    callback_query: {
      id: "cb-1", from: { id: 10, is_bot: false, first_name: "User" },
      chat_instance: "instance", data: "numbers:new",
      message: { message_id: 1, date: 1, chat: { id: 10, type: "private" }, text: "Numbers" },
    },
  });
  const editMsg = calls.find((c) => c.method === "editMessageText");
  assert.ok(editMsg, "Should show country selection in random mode");
  assert.ok(editMsg.payload.text.includes("страну") || /country/i.test(editMsg.payload.text));
});

test("real mode rejects random number generation request", async () => {
  const { bot, calls, db } = createBotWithMode("real");
  await db.upsertUser({ id: 10, first_name: "User" }, 10, "ru");
  await bot.handleUpdate({
    update_id: 1,
    callback_query: {
      id: "cb-1", from: { id: 10, is_bot: false, first_name: "User" },
      chat_instance: "instance", data: "numbers:new",
      message: { message_id: 1, date: 1, chat: { id: 10, type: "private" }, text: "Numbers" },
    },
  });
  const sent = calls.find((c) => c.method === "sendMessage");
  assert.ok(sent, "Real mode should send a rejection message");
  assert.match(sent.payload.text, /not available|недоступна/i);
});

test("real mode shows phone binding in numbers menu", async () => {
  const { bot, calls, db } = createBotWithMode("real");
  await db.upsertUser({ id: 10, first_name: "User" }, 10, "ru");
  await bot.handleUpdate({
    update_id: 1,
    callback_query: {
      id: "cb-1", from: { id: 10, is_bot: false, first_name: "User" },
      chat_instance: "instance", data: "menu:numbers",
      message: { message_id: 1, date: 1, chat: { id: 10, type: "private" }, text: "Menu" },
    },
  });
  const sent = calls.find((c) => c.method === "sendMessage");
  assert.ok(sent, "Real mode numbers menu should show phone binding prompt");
  assert.match(sent.payload.text, /Phone binding|Привязка номера/i);
});

test("random mode numbers menu shows number list", async () => {
  const { bot, calls, db } = createBotWithMode("random");
  await db.upsertUser({ id: 10, first_name: "User" }, 10, "ru");
  await db.createNumber(10, 10, "free", "RU", false);
  await bot.handleUpdate({
    update_id: 1,
    callback_query: {
      id: "cb-1", from: { id: 10, is_bot: false, first_name: "User" },
      chat_instance: "instance", data: "menu:numbers",
      message: { message_id: 1, date: 1, chat: { id: 10, type: "private" }, text: "Menu" },
    },
  });
  const edit = calls.find((c) => c.method === "editMessageText");
  assert.ok(edit, "Random mode numbers menu should show number list");
  assert.match(edit.payload.text, /\+7/);
});

test("real mode shop hides +888 number products", async () => {
  const { bot, calls, db } = createBotWithMode("real");
  await db.upsertUser({ id: 10, first_name: "User" }, 10, "ru");
  await bot.handleUpdate({
    update_id: 1,
    callback_query: {
      id: "cb-1", from: { id: 10, is_bot: false, first_name: "User" },
      chat_instance: "instance", data: "shop:number",
      message: { message_id: 1, date: 1, chat: { id: 10, type: "private" }, text: "Shop" },
    },
  });
  const sent = calls.find((c) => c.method === "sendMessage");
  assert.ok(sent, "Real mode should reject number shop access");
  assert.match(sent.payload.text, /not available|недоступна/i);
});

test("real mode allows shop for non-number products", async () => {
  const { bot, calls, db } = createBotWithMode("real");
  await db.upsertUser({ id: 10, first_name: "User" }, 10, "ru");
  await bot.handleUpdate({
    update_id: 1,
    callback_query: {
      id: "cb-1", from: { id: 10, is_bot: false, first_name: "User" },
      chat_instance: "instance", data: "shop:premium",
      message: { message_id: 1, date: 1, chat: { id: 10, type: "private" }, text: "Shop" },
    },
  });
  const edit = calls.find((c) => c.method === "editMessageText");
  assert.ok(edit, "Real mode should allow Premium shop");
  assert.match(edit.payload.text, /Choose|Выберите/i);
});
