#!/usr/bin/env node
// Only the disposable image-smoke backend and explicitly seeded synthetic IDs.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const { chromium } = require('/opt/artex-tools/node_modules/playwright');
const base = new URL(process.env.BASE_URL);
const token = process.env.AUTH_TOKEN;
const taskId = Number(process.env.LIFECYCLE_TASK_ID);
assert(token && Number.isSafeInteger(taskId) && taskId > 0);
async function main() {
  const browser = await chromium.launch({args:['--no-sandbox','--disable-dev-shm-usage']});
  const context = await browser.newContext({viewport:{width:1440,height:1100},acceptDownloads:true});
  await context.addCookies([{name:'artex_token',value:token,url:base.origin}]);
  await context.addInitScript(t=>localStorage.setItem('artex_token',t),token);
  await context.route('**/*',route=>{const u=new URL(route.request().url());if(u.origin!==base.origin)return route.abort();if(u.pathname==='/api/update/check')return route.fulfill({json:{current:'smoke',has_update:false,mode:'docker'}});return route.continue();});
  const page=await context.newPage();page.setDefaultTimeout(15000);const errors=[];page.on('pageerror',e=>errors.push(e.message));
  const api=async(path,method='GET',data)=>{const r=await context.request.fetch(new URL('/api'+path,base).href,{method,headers:{Authorization:'Bearer '+token},...(data===undefined?{}:{data})});assert(r.ok(),`${method} ${path}: ${r.status()} ${await r.text()}`);return r.json();};
  try {
    const resourceName=`Lifecycle synthetic resource ${taskId}`;
    const resource=await api('/resources','POST',{name:resourceName,kind:'file',description:'Disposable fixture',access_mode:'global',enabled:true,tags:[]});
    await page.goto(new URL(`/function/tasks/detail?id=${taskId}`,base).href);
    const secureContext=await page.evaluate(()=>window.isSecureContext);
    if(process.env.EXPECT_HTTP_ORIGIN==='1')assert.equal(secureContext,false,'fixture must cover an ordinary HTTP origin');
    await page.getByRole('tab',{name:'记录与收尾',exact:true}).click();
    await page.getByRole('heading',{name:'Worker 预算'}).waitFor();
    await page.getByLabel('累计 Token 上限',{exact:true}).fill('1000000');
    await page.getByRole('button',{name:'保存软预算',exact:true}).click();
    await page.getByText(/已记录 .*1,000,000/).waitFor();
    const beforeConflict=await api(`/tasks/${taskId}/token-budget`);
    await api(`/tasks/${taskId}/token-budget`,'POST',{limit:2000000,expected_revision:beforeConflict.revision});
    await page.getByLabel('累计 Token 上限',{exact:true}).fill('0');
    await page.getByRole('button',{name:'保存软预算',exact:true}).click();
    await page.getByRole('alert').filter({hasText:/预算已变更/}).waitFor();
    await page.getByRole('button',{name:'刷新累计用量',exact:true}).click();
    await page.getByLabel('累计 Token 上限',{exact:true}).evaluate(async input=>{for(let i=0;i<100&&input.value!=='2000000';i++)await new Promise(r=>setTimeout(r,50));if(input.value!=='2000000')throw Error('budget refresh failed');});
    await page.getByLabel('累计 Token 上限',{exact:true}).fill('0');
    await page.getByRole('button',{name:'保存软预算',exact:true}).click();
    await page.getByText(/已记录 .*不限/).waitFor();
    await page.getByRole('button',{name:'追加预算',exact:true}).click();
    await page.getByLabel('执行轮数',{exact:true}).fill('20');
    await page.getByLabel('收尾轮数',{exact:true}).fill('10');
    await page.getByLabel('追加预算理由').fill('Synthetic pending verification');
    await page.getByRole('button',{name:'确认追加',exact:true}).click();
    await page.getByText(/执行 150\/170/).waitFor();
    await page.getByRole('button',{name:'登记部署',exact:true}).click();
    const dialog=page.getByRole('dialog');
    for(const [label,value] of Object.entries({'名称':'Synthetic fixture','类型':'file','部署位置':'isolated test workspace','准确标识 / 路径':'fixture.txt','撤除与验证方法':'Remove exact synthetic file then verify absence','关联资源 ID（可选）':String(resource.id)}))await dialog.getByLabel(label,{exact:true}).fill(value);
    await dialog.getByRole('button',{name:'保存登记',exact:true}).click();
    await page.getByText('待处理部署 · 1',{exact:true}).waitFor();
    await page.getByRole('button',{name:'更新记录',exact:true}).click();
    await dialog.getByLabel('部署状态').selectOption('removed');
    await dialog.getByLabel('实际验证结果 / 保留依据',{exact:true}).fill('Synthetic fixture was never deployed externally; absent in disposable workspace.');
    await dialog.getByRole('button',{name:'保存登记',exact:true}).click();
    await page.getByText(/已验证撤除/).waitFor();
    assert.equal((await api(`/tasks/${taskId}/deployments`))[0].revision,2);
    await page.getByRole('button',{name:'导出完整记录',exact:true}).click();
    await page.getByRole('button',{name:'开始生成',exact:true}).click();
    const downloadButton=page.getByRole('link',{name:'下载 ZIP',exact:true});await downloadButton.waitFor({timeout:60000});
    const [download]=await Promise.all([page.waitForEvent('download'),downloadButton.click()]);assert.equal(await download.failure(),null);
    const ready=(await api(`/task-record-exports?task_id=${taskId}`)).find(j=>j.state==='ready');assert(ready.sha256 && ready.files>0);
    if(process.env.SCREENSHOT_DIR){await fs.mkdir(process.env.SCREENSHOT_DIR,{recursive:true});await page.screenshot({path:process.env.SCREENSHOT_DIR+'/handoff.png',fullPage:true});await download.saveAs(process.env.SCREENSHOT_DIR+'/browser-records.zip');}
    await page.goto(new URL('/function/resources',base).href);
    await page.getByText(resourceName,{exact:true}).first().click();
    await page.getByRole('tab',{name:'引用与删除',exact:true}).click();
    await page.getByLabel('资源删除原因').fill('Synthetic resource retired after verification');
    await page.getByRole('button',{name:'确认删除登记',exact:true}).click();
    await page.getByRole('heading',{name:resourceName,exact:true}).waitFor({state:'detached'});
    assert.equal((await api('/resources?q='+encodeURIComponent(resourceName))).some(r=>r.id===resource.id),false);
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({lifecycle:{task_budget_cas:true,budget_grant:true,deployment_create_update:true,record_export_download:true,resource_delete:true,browser_errors:0,secure_context:secureContext}}));
  } catch(e) {console.error(JSON.stringify({browser_errors:errors}));console.error((await page.locator('body').innerText()).slice(0,5000));throw e;}
  finally {await browser.close();}
}
main().catch(e=>{console.error(e);process.exitCode=1;});
