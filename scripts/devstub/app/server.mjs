import { createServer } from "node:http";
createServer((req, res) => {
  if (req.url === "/healthz") {
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ component: "app-stub", status: "ok" }));
  } else { res.statusCode = 404; res.end(); }
}).listen(7400, "127.0.0.1", () => console.log("app-stub on :7400"));
