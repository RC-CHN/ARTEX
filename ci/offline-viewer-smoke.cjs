// Synthetic artifacts only. Run with the ARTEX image's bundled browser and
// --network none; file:// must work without fetch/CORS or an HTTP server.
const {chromium}=require('/opt/artex-tools/node_modules/playwright');
const assert=require('node:assert/strict');
(async()=>{
 const browser=await chromium.launch({args:['--no-sandbox','--disable-dev-shm-usage']});
 try {
  const page=await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
  page.setDefaultTimeout(15000);
  const root=process.env.ARTEX_OFFLINE_ROOT||'file:///artifacts/offline-large/bundle';
  await page.goto(root+'/tasks/1/transcripts/fixture.html');
  await page.locator('#status').filter({hasText:'第 1 页'}).waitFor();
  assert.equal(await page.locator('#records details').count(),200);
  await page.getByRole('button',{name:'下一页'}).click();
  await page.locator('#status').filter({hasText:'第 2 页'}).waitFor();
  await page.getByLabel('搜索记录').fill('fixture-1004');
  await page.locator('#status').filter({hasText:'共 1 条'}).waitFor();
  assert.equal(await page.locator('#records details').count(),1);
  assert.match(await page.locator('#records').textContent(),/synthetic record 1004/);
  await page.getByLabel('搜索记录').fill('<script>bad()</script>');
  await page.locator('#status').filter({hasText:'本页 200 条'}).waitFor();
  // Rapid replacement must not let an older asynchronous search win.
  for(const q of ['fixture-0001','fixture-0500','fixture-1000'])await page.getByLabel('搜索记录').fill(q);
  await page.locator('#status').filter({hasText:'共 1 条'}).waitFor();
  await page.locator('#records').filter({hasText:'synthetic record 1000'}).waitFor();
  await page.goto(root+'/index.html');
  await page.locator('#status').filter({hasText:'第 1 页'}).waitFor();
  await page.getByLabel('筛选文件').fill('fixture.html');
  await page.locator('#status').filter({hasText:'共 1 条'}).waitFor();
  assert.equal(await page.locator('article a[download]').count(),0);
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({offline:true,records:1005,dom_limit:200,global_search:true,rapid_search:true,escaped_html:true,browser_errors:0}));
 }finally{await browser.close()}
})().catch(e=>{console.error(e);process.exitCode=1});
