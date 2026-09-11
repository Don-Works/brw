package snapshot

import (
	"encoding/json"
	"fmt"
)

// StorageScript reads and writes localStorage / sessionStorage.
//
// Web storage is ordinary page state that agents legitimately need to inspect
// (feature flags, onboarding state, a cached draft) and occasionally seed for a
// test. It is deliberately scoped to the two Storage APIs: it is NOT a cookie
// or credential surface, and it never touches HttpOnly state.
const StorageScript = `(function(kind, action, key, value){
  var store = kind === 'session' ? window.sessionStorage : window.localStorage;
  if(!store) throw new Error(kind + 'Storage is unavailable on this origin');
  switch(action){
    case 'get':
      if(key) return {key: key, value: store.getItem(key)};
      var items = {};
      for(var i=0;i<store.length;i++){ var k = store.key(i); items[k] = store.getItem(k); }
      return {items: items, count: store.length};
    case 'set':
      if(!key) throw new Error('storage set requires a key');
      store.setItem(key, value === null || value === undefined ? '' : String(value));
      return {key: key, value: store.getItem(key)};
    case 'remove':
      if(!key) throw new Error('storage remove requires a key');
      store.removeItem(key);
      return {key: key, removed: true};
    case 'clear':
      var cleared = store.length;
      store.clear();
      return {cleared: cleared};
    default:
      throw new Error('unknown storage action ' + JSON.stringify(action));
  }
})`

// BuildStorageExpression renders StorageScript for one operation.
func BuildStorageExpression(kind, action, key, value string) string {
	kindJSON, _ := json.Marshal(kind)
	actionJSON, _ := json.Marshal(action)
	keyJSON, _ := json.Marshal(key)
	valueJSON, _ := json.Marshal(value)
	return fmt.Sprintf("%s(%s,%s,%s,%s)", StorageScript, kindJSON, actionJSON, keyJSON, valueJSON)
}
