import redis
import argparse

parser = argparse.ArgumentParser(
                    prog = 'RBR',
                    description = 'Redis backup and recovery script',
                    epilog = '-r to recovery or -b to backup')

parser.add_argument('-b', '--backup', help='backup to file')
parser.add_argument('-r', '--recovery', help='recovery from file')
parser.add_argument('-db', '--db', help='db number', default=0)
parser.add_argument('-p', '--prefix', help='backup key prefix', default='')
args = parser.parse_args()

r = redis.Redis(host='localhost', port=6379, db=args.db)
if args.backup != None:
    f = open(args.backup, 'w')
    for key in r.scan_iter(args.prefix + "*"):
        # print(key, r.get(key))
        f.writelines(key.decode("utf-8") )
        f.writelines('\n')
        f.writelines(r.get(key).decode("utf-8") )
        f.writelines('\n')
    f.close()
    
if args.recovery != None:
    f = open(args.recovery, 'r')
    key = f.readline()[:-1]
    while len(key) != 0:
        value = f.readline()[:-1]
        r.set(key, value)
        key = f.readline()[:-1]
    f.close()
