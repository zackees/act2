const fs = require('fs');
if (process.getuid() === 0) throw new Error('Node action inherited root');
fs.writeFileSync(`${process.env.HOME}/node-action-writable`, 'ordinary user');
console.log(`Node action uid=${process.getuid()}`);
