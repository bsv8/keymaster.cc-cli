import { PrivateKey, Utils } from '@bsv/sdk';
import { parse, unlock, exportPrivateKey, recommendedParameters } from 'keyhold';

const order = BigInt('0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141');
function validate(bytes) {
  const n = BigInt(`0x${Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('')}`);
  if (bytes.length !== 32 || n === 0n || n >= order) throw new Error('私钥必须是有效的 32 字节 secp256k1 私钥');
  return bytes;
}
export function generateKey() {
  for (;;) {
    const bytes = crypto.getRandomValues(new Uint8Array(32));
    try { return validate(bytes); } catch { bytes.fill(0); }
  }
}
export async function parseMaterial(format, text, password) {
  if (format === 'keyhold') {
    const document = parse(text);
    const result = await unlock(document, password);
    try { return { bytes: validate(result.privateKey), label: document.label, format: 'KeyHold', network: null }; }
    catch (error) { result.privateKey.fill(0); throw error; }
  }
  if (format === 'hex') {
    const hex = text.trim().replace(/^0x/i, '');
    if (!/^[a-f\d]{64}$/i.test(hex)) throw new Error('Hex 必须是 64 个十六进制字符');
    const bytes = Uint8Array.from(hex.match(/../g), byte => parseInt(byte, 16));
    try { return { bytes: validate(bytes), label: '', format: 'Hex', network: null }; }
    catch (error) { bytes.fill(0); throw error; }
  }
  const decoded = Utils.fromBase58Check(text.trim());
  try {
    if (![0x80, 0xef].includes(decoded.prefix[0]) || decoded.prefix.length !== 1 ||
      ![32, 33].includes(decoded.data.length) || (decoded.data.length === 33 && decoded.data[32] !== 1)) throw new Error('无效的 BSV WIF');
    const bytes = Uint8Array.from(decoded.data.slice(0, 32));
    try { return { bytes: validate(bytes), label: '', format: 'WIF', network: decoded.prefix[0] === 0xef ? 'test' : 'main' }; }
    catch (error) { bytes.fill(0); throw error; }
  } finally { decoded.data.fill(0); }
}
export function describeKey(bytes, network) {
  const key = new PrivateKey(Array.from(bytes));
  try { return { publicKey: key.toPublicKey().toString(), address: key.toAddress(network === 'test' ? 'testnet' : 'mainnet') }; }
  finally { key.words.fill(0); }
}
export function encryptMaterial(bytes, password, label) {
  return exportPrivateKey({ privateKey: bytes, password, label, parameters: recommendedParameters() });
}
