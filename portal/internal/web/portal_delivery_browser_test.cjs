"use strict";
// Reuse the existing hydrated Pi-page fixture and its strict request inventory.
process.env.PI_MESSAGE_ONLY = "1";
process.env.PI_DURABLE_ONLY = "1";
require("./message_delivery_browser_test.cjs");
