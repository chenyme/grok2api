import sys, asyncio, os, gc
sys.path.insert(0, '.')
from pathlib import Path
from app.control.account.backends.local import LocalAccountRepository
from app.control.account.commands import AccountUpsert
from app.control.account.refresh import AccountRefreshService

files = [
    (r"F:\聊天记录\grok_sso (190JF).txt",           "190JF"),
    (r"F:\聊天记录\grok_sso (190JF) (1).txt",       "190JF(1)"),
    (r"F:\聊天记录\sso10.txt",                       "sso10"),
    (r"F:\聊天记录\grok_sso (19).txt",               "19"),
    (r"F:\聊天记录\grok_sso (10).txt",               "10"),
    (r"F:\聊天记录\grok_sso (56).txt",               "56"),
    (r"F:\聊天记录\grok_sso (64).txt",               "64"),
    (r"F:\聊天记录\accounts_ORDERXH4U0IUDDN_sso(1).txt", "ORDX(1)"),
    (r"F:\聊天记录\accounts_ORDERXH4U0IUDDN_sso.txt",     "ORDX"),
]

async def main():
    db_path = Path("data/per_file.db")
    for ext in ("", "-wal", "-shm"):
        p = Path(str(db_path) + ext)
        if p.exists(): os.unlink(p)

    repo = LocalAccountRepository(db_path)
    await repo.initialize()
    svc = AccountRefreshService(repo)

    print(f"{'File':<12} {'Total':>5} {'Super':>5} {'Basic':>5} {'NoAID':>5} {'UniqID':>6}  Quota(auto/fast/expert)")
    print("-" * 88)

    cum_s = set()
    cum_b = set()
    grand_total = 0

    for filepath, name in files:
        with open(filepath, "r") as f:
            cookies = [l.strip() for l in f if l.strip() and "eyJ" in l]

        upserts = [AccountUpsert(token=c, pool="auto") for c in cookies]
        await repo.upsert_accounts(upserts)

        tokens = [u.token for u in upserts]
        records = await repo.get_accounts(tokens)
        for r in records:
            if not r.account_id:
                await svc._refresh_subscription(r)

        records = await repo.get_accounts(tokens)
        for r in records:
            if r.pool in ("basic", "auto"):
                await svc._refresh_one(r, apply_fallback=True)

        records = await repo.get_accounts(tokens)
        s_cnt = sum(1 for r in records if r.pool == "super")
        b_cnt = sum(1 for r in records if r.pool == "basic")
        h_cnt = sum(1 for r in records if r.pool == "heavy")
        noaid = sum(1 for r in records if not r.account_id)
        s_ids = {r.account_id for r in records if r.pool == "super" and r.account_id}
        b_ids = {r.account_id for r in records if r.pool == "basic" and r.account_id}

        auto_q = sum(r.quota_set().auto.remaining for r in records if r.pool == "super")
        fast_q = sum(r.quota_set().fast.remaining for r in records)
        expert_q = sum(r.quota_set().expert.remaining for r in records if r.pool == "super")

        cum_s.update(s_ids)
        cum_b.update(b_ids)
        grand_total += len(cookies)

        h = f" H:{h_cnt}" if h_cnt else ""
        print(f"{name:<12} {len(cookies):>5} {s_cnt:>5} {b_cnt:>5} {noaid:>5} {len(s_ids)+len(b_ids):>6}  {auto_q}/{fast_q}/{expert_q}{h}")

        gc.collect()

    print("-" * 88)
    print(f"{'CUMULATIVE':<12} {grand_total:>5} {len(cum_s):>5} {len(cum_b):>5} {'':>5} {len(cum_s)+len(cum_b):>6}")

    await repo.close()
    for ext in ("", "-wal", "-shm"):
        p = Path(str(db_path) + ext)
        if p.exists(): os.unlink(p)

asyncio.run(main())
