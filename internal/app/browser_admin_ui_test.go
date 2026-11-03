package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 执行真实管理页脚本及账号操作，验证登录流程和选择接口而不需要真实 Google。
func TestBrowserAdminPageActions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 Node，无法自动执行管理页脚本")
	}
	html, err := os.ReadFile("admin_ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(html, []byte("<script>"))
	end := bytes.LastIndex(html, []byte("</script>"))
	if start < 0 || end < 0 {
		t.Fatal("管理页没有应用脚本")
	}
	script := append([]byte(""), html[start+len("<script>"):end]...)
	fixture := []byte(`
const assert = require('node:assert/strict');
const calls = [];
global.localStorage = { getItem: () => null, setItem: () => {} };
global.fetch = async () => ({ok:false,status:503,json:async()=>({})});
global.document = {
  documentElement: {}, title:'',
  getElementById: () => ({innerHTML:'',focus(){},addEventListener(){}}),
  querySelectorAll: () => [], querySelector: () => ({classList:{}})
};
global.MutationObserver = class { observe(){} disconnect(){} };
global.requestAnimationFrame = () => {};
global.window = global;
`)
	fixture = append(fixture, script...)
	fixture = append(fixture, []byte(`
async function runAccountUITest() {
  flash = () => {};
  refreshPage = async () => {};
  modalForm = async () => ({label:'测试',note:''});
  modalConfirm = async () => true;
  fetch = async (path, options) => {
    calls.push({path,method:options.method,body:options.body});
    const data = path === 'api/accounts' && options.method === 'POST' ? {id:12}
      : path === 'api/accounts' ? {items:[{id:12,label:'测试',email:'owner@example.com',status:'enabled',login_state:'ready',profile_id:'abc',key_cookies:[],cookie_count:1,fail_count:0}],account_id:0,total:1,enabled:1}
      : {ok:true};
    return {status:200,json:async()=>data};
  };
  await addBrowserAccount();
  assert.equal(calls[0].path,'api/accounts');
  assert.equal(calls[1].path,'api/accounts/12/login');
  assert.equal(calls[0].method,'POST');
  await selectBrowserAccount(12);
  assert.equal(calls[2].path,'api/accounts/selection');
  assert.equal(JSON.parse(calls[2].body).account_id,12);
  const html = await pages.cookies();
  assert.ok(html.includes('owner@example.com'));
  assert.ok(html.includes('账号池'));
  assert.ok(html.includes('使用此账号'));
  const pending = {...(await api('api/accounts')).items[0],login_state:'logging_in'};
  fetch = async () => ({status:200,json:async()=>({items:[pending],account_id:0,total:1,enabled:1})});
  assert.ok((await pages.cookies()).includes('完成登录'));
	modalForm = async (options) => {
	  const field = options.fields.find(field => field.name === 'proxy_id');
	  assert.ok(field.placeholder.includes('0 自动选择出口'));
	  assert.ok(options.desc.includes('已有绑定优先使用'));
	  return {label:'测试',note:'',proxy_id:'0'};
	};
	fetch = async (path, options) => {
	  calls.push({path,method:options ? options.method : undefined,body:options ? options.body : undefined});
	  if (path === 'api/proxies/probe') return {status:200,json:async()=>({items:[{id:1,ok:true}]})};
	  if (path === 'api/proxies/auto-bind') return {status:200,json:async()=>({result:{bound_accounts:1,healthy_proxies:1}})};
	  if (path === 'api/proxies/clean-unavailable') return {status:200,json:async()=>({ok:true,deleted:1})};
	  if (path === 'api/proxies/import-clash') return {status:200,json:async()=>({ok:true,imported:68,total:68})};
	  return {status:200,json:async()=>({ok:true})};
	};
	await editCookieAcct(12,'测试','');
	assert.equal(JSON.parse(calls[calls.length-1].body).proxy_id,0);
	global.confirm = () => true;
	global.alert = (msg) => { throw new Error(msg); };
	await probeAllProxies();
	await autoBindBestProxies();
	await importClashPorts();
	await deleteUnavailableProxies();
}
runAccountUITest().catch(error => { process.stderr.write(error.stack+'\n');process.exitCode=1; });
`)...)
	path := filepath.Join(t.TempDir(), "account-ui-test.cjs")
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("管理页操作失败: %v\n%s", err, output)
	}
}
